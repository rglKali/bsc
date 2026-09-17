package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

func newWithdrawal(wallet uuid.UUID, amount int64, key string) Withdrawal {
	return Withdrawal{
		ID: uuid.New(), Wallet: wallet, Reason: ReasonPayout, Destination: addr(0x99),
		Amount: wei(amount), Status: WithdrawalPending, IdempotencyKey: key,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

// A retry after a timeout must replay the original rather than pay twice, which
// over HTTP is the only thing standing between a dropped response and a double
// spend.
func TestWithdrawalIdempotencyLookup(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	wd := newWithdrawal(w.ID, 250, "order-4417")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.WithdrawalByKey("order-4417")
		if err != nil || !ok {
			t.Fatalf("by key: ok=%v err=%v", ok, err)
		}
		if got.ID != wd.ID {
			t.Fatalf("key resolved to %s, want %s", got.ID, wd.ID)
		}
		if _, ok, err := tx.WithdrawalByKey("never-used"); err != nil || ok {
			t.Fatalf("unknown key: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustBeClean(t, verify(t, s))
}

// The idempotency namespace is global now, not per app: one caller owns the
// service and must keep its own keys unique (§37).
func TestIdempotencyKeysAreGlobal(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x21), common.Address{}, 1000)
	b := seedWalletAt(t, s, "b", addr(0x22), common.Address{}, 1000)

	first := newWithdrawal(a.ID, 10, "shared")
	second := newWithdrawal(b.ID, 20, "shared")
	update(t, s, func(tx *Tx) error {
		if err := tx.PutWithdrawal(first); err != nil {
			return err
		}
		return tx.PutWithdrawal(second)
	})

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.WithdrawalByKey("shared")
		if err != nil || !ok {
			return err
		}
		if got.ID != second.ID {
			t.Fatal("a key reused across wallets did not resolve to the later writer")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWithdrawalsWithoutAKeyAreStillStored(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	wd := newWithdrawal(w.ID, 100, "")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.Withdrawal(wd.ID)
		if err != nil || !ok {
			t.Fatalf("lookup: ok=%v err=%v", ok, err)
		}
		if got.Amount.Cmp(wei(100)) != 0 {
			t.Fatalf("amount = %s", got.Amount)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustBeClean(t, verify(t, s))
}

// `confirmed` is the only terminal status, so the open set is exactly what is
// still pending — which is both what the rules scan and what the caller polls.
func TestOpenSetTracksTerminalStatus(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	wd := newWithdrawal(w.ID, 100, "k")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	if err := s.View(func(tx *Tx) error {
		open, err := tx.OpenWithdrawals()
		if err != nil {
			return err
		}
		if len(open) != 1 {
			t.Fatalf("open = %d, want 1", len(open))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWithdrawal(wd.ID, func(x *Withdrawal) error {
			x.Status = WithdrawalConfirmed
			x.TxHash = hash(0x42)
			return nil
		})
		return err
	})

	if err := s.View(func(tx *Tx) error {
		open, err := tx.OpenWithdrawals()
		if err != nil {
			return err
		}
		if len(open) != 0 {
			t.Fatalf("open = %d after confirmation, want 0", len(open))
		}
		all, err := tx.Withdrawals(0)
		if err != nil {
			return err
		}
		if len(all) != 1 {
			t.Fatalf("history = %d, want 1 — a confirmed withdrawal is kept forever", len(all))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustBeClean(t, verify(t, s))
}

// Committed is the overdraft guard's left-hand side: what this wallet has
// promised and not yet paid (§39).
func TestCommittedSumsOnlyPendingWithdrawalsForOneWallet(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x21), common.Address{}, 5000)
	b := seedWalletAt(t, s, "b", addr(0x22), common.Address{}, 5000)

	update(t, s, func(tx *Tx) error {
		if err := tx.PutWithdrawal(newWithdrawal(a.ID, 100, "")); err != nil {
			return err
		}
		if err := tx.PutWithdrawal(newWithdrawal(a.ID, 250, "")); err != nil {
			return err
		}
		if err := tx.PutWithdrawal(newWithdrawal(b.ID, 900, "")); err != nil {
			return err
		}
		done := newWithdrawal(a.ID, 4000, "")
		done.Status = WithdrawalConfirmed
		return tx.PutWithdrawal(done)
	})

	if err := s.View(func(tx *Tx) error {
		got, err := tx.Committed(a.ID)
		if err != nil {
			return err
		}
		if got.Int64() != 350 {
			t.Fatalf("committed(a) = %s, want 350", got)
		}
		got, err = tx.Committed(b.ID)
		if err != nil {
			return err
		}
		if got.Int64() != 900 {
			t.Fatalf("committed(b) = %s, want 900", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMutateWithdrawalRefusesImmutableFields(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	wd := newWithdrawal(w.ID, 100, "k")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	cases := map[string]func(*Withdrawal){
		"id":              func(x *Withdrawal) { x.ID = uuid.New() },
		"wallet":          func(x *Withdrawal) { x.Wallet = uuid.New() },
		"idempotency key": func(x *Withdrawal) { x.IdempotencyKey = "other" },
		"created_at":      func(x *Withdrawal) { x.CreatedAt = time.Now().Add(time.Hour) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(func(tx *Tx) error {
				_, err := tx.MutateWithdrawal(wd.ID, func(x *Withdrawal) error {
					mutate(x)
					return nil
				})
				return err
			})
			if !errors.Is(err, ErrImmutable) {
				t.Fatalf("err = %v, want ErrImmutable", err)
			}
		})
	}
}

func TestWithdrawalsAreListedMostRecentFirst(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 10_000)
	for i := range 3 {
		wd := newWithdrawal(w.ID, int64(i+1), "")
		wd.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Second)
		update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })
	}

	if err := s.View(func(tx *Tx) error {
		list, err := tx.WalletWithdrawals(w.ID, 0)
		if err != nil {
			return err
		}
		if len(list) != 3 {
			t.Fatalf("listed %d, want 3", len(list))
		}
		for i, wd := range list {
			if want := int64(3 - i); wd.Amount.Int64() != want {
				t.Fatalf("position %d has amount %s, want %d (newest first)", i, wd.Amount, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMutateMissingWithdrawal(t *testing.T) {
	s := open(t)
	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutateWithdrawal(uuid.New(), func(*Withdrawal) error { return nil })
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// Go's zero value is a valid uint8, so a construction site that forgets the
// reason compiles silently and writes a debit that explains nothing. The store
// is the only place that can catch it (§42).
func TestWithdrawalMustNameAReason(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)

	err := s.Update(func(tx *Tx) error {
		return tx.PutWithdrawal(Withdrawal{
			ID: uuid.New(), Wallet: w.ID, // Reason omitted
			Destination: addr(0x99), Amount: wei(10),
			Status: WithdrawalPending, CreatedAt: time.Now().UTC(),
		})
	})
	if err == nil {
		t.Fatal("a debit with no reason was accepted")
	}
	if !strings.Contains(err.Error(), "no reason") {
		t.Fatalf("err = %v, want it to name the missing reason", err)
	}
}

// Anything unrecognised is a record nothing in the service could have written,
// so the audit reports it as such. It has to be written directly, bypassing the
// write-side refusal, because there is no other way to produce one.
func TestAuditFlagsAnUnrecognisedReason(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)

	update(t, s, func(tx *Tx) error {
		wd := Withdrawal{
			ID: uuid.New(), Wallet: w.ID, Reason: DebitReason(99),
			Destination: addr(0x99), Amount: wei(10),
			Status: WithdrawalPending, CreatedAt: time.Now().UTC(),
		}
		return put(tx, bWithdrawal, wd.ID[:], wd.encode)
	})

	found := findingsOfKind(verify(t, s), "ownership")
	if len(found) != 1 {
		t.Fatalf("ownership findings = %d, want 1", len(found))
	}
	if !strings.Contains(found[0].Detail, "reason 99") {
		t.Fatalf("detail = %q, want it to name the value", found[0].Detail)
	}
}
