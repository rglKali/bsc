package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

func newPending(wallet WalletID, amount int64, key string) Pending {
	return Pending{
		ID: uuid.New(), Wallet: wallet, Reason: ReasonPayout, Destination: addr(0x99),
		Amount: wei(amount), IdempotencyKey: key,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
}

// A retry after a timeout must replay the original rather than pay twice, which
// over HTTP is the only thing standing between a dropped response and a double
// spend. The key has to resolve whichever keyspace the original is in (§51).
func TestIdempotencyLookupFindsEitherHalf(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	p := newPending(w.ID, 250, "order-4417")
	update(t, s, func(tx *Tx) error { return tx.PutPending(p) })

	if err := s.View(func(tx *Tx) error {
		got, done, err := tx.DebitByKey("order-4417")
		if err != nil {
			return err
		}
		if got == nil || done != nil || got.ID != p.ID {
			t.Fatalf("while pending: got %+v / %+v, want the promise", got, done)
		}
		if _, _, err := tx.DebitByKey("never-used"); err != nil {
			t.Fatalf("unknown key: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The same key must still resolve once the promise has become a fact.
	update(t, s, func(tx *Tx) error {
		_, err := tx.Settle(p.ID, hash(0x42), 100, time.Now().UTC())
		return err
	})
	if err := s.View(func(tx *Tx) error {
		got, done, err := tx.DebitByKey("order-4417")
		if err != nil {
			return err
		}
		if done == nil || got != nil || done.ID != p.ID {
			t.Fatalf("after settling: got %+v / %+v, want the fact", got, done)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustBeClean(t, verify(t, s))
}

// The idempotency namespace is global, not per wallet: one caller owns the
// service and must keep its own keys unique (§37).
func TestIdempotencyKeysAreGlobal(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x21), common.Address{}, 1000)
	b := seedWalletAt(t, s, "b", addr(0x22), common.Address{}, 1000)

	first := newPending(a.ID, 100, "shared")
	second := newPending(b.ID, 200, "shared")
	update(t, s, func(tx *Tx) error {
		if err := tx.PutPending(first); err != nil {
			return err
		}
		return tx.PutPending(second)
	})

	if err := s.View(func(tx *Tx) error {
		got, _, err := tx.DebitByKey("shared")
		if err != nil {
			return err
		}
		if got == nil || got.ID != second.ID {
			t.Fatal("a key reused across wallets did not resolve to the later record")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Settling MOVES the record. The promise is gone from state/ and the fact is in
// log/, in one transaction — which is what makes "which bucket it is in" a
// status that cannot disagree with anything (§51).
func TestSettlingMovesTheRecordBetweenKeyspaces(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	p := newPending(w.ID, 100, "k")
	update(t, s, func(tx *Tx) error { return tx.PutPending(p) })

	if err := s.View(func(tx *Tx) error {
		open, err := tx.OpenPending()
		if err != nil {
			return err
		}
		if len(open) != 1 {
			t.Fatalf("open = %d, want 1", len(open))
		}
		if _, ok, err := tx.Withdrawal(p.ID); err != nil || ok {
			t.Fatalf("a promise is already in the log (ok=%v err=%v)", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	settledAt := time.Now().UTC()
	update(t, s, func(tx *Tx) error {
		wd, err := tx.Settle(p.ID, hash(0x42), 4821, settledAt)
		if err != nil {
			return err
		}
		if wd.Amount.Cmp(p.Amount) != 0 || wd.Reason != p.Reason || wd.TxHash != hash(0x42) {
			t.Fatalf("settled record = %+v, does not carry the promise", wd)
		}
		return nil
	})

	if err := s.View(func(tx *Tx) error {
		open, err := tx.OpenPending()
		if err != nil {
			return err
		}
		if len(open) != 0 {
			t.Fatalf("open = %d after settling, want 0", len(open))
		}
		if _, ok, err := tx.Pending(p.ID); err != nil || ok {
			t.Fatalf("the promise outlived settlement (ok=%v err=%v)", ok, err)
		}
		wd, ok, err := tx.Withdrawal(p.ID)
		if err != nil || !ok {
			t.Fatalf("the fact is not in the log (ok=%v err=%v)", ok, err)
		}
		if wd.Block != 4821 || !wd.SettledAt.Equal(settledAt) {
			t.Fatalf("settled record = %+v, want block and settled_at recorded", wd)
		}
		// And it is on the feed a caller polls.
		feed, _, err := tx.SettledSince(Settled{}, 0)
		if err != nil {
			return err
		}
		if len(feed) != 1 || feed[0].ID != p.ID {
			t.Fatalf("settled feed = %+v, want the one debit", feed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustBeClean(t, verify(t, s))
}

func TestSettlingSomethingThatIsNotPending(t *testing.T) {
	s := open(t)
	err := s.Update(func(tx *Tx) error {
		_, err := tx.Settle(uuid.New(), hash(1), 1, time.Now())
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// Committed is the overdraft guard's left-hand side: what this wallet has
// promised and not yet paid (§39). Splitting the keyspaces made it a scan of
// exactly the promises — a settled debit cannot leak into it, because it is not
// in the bucket any more (§51).
func TestCommittedSumsOnlyPromisesForOneWallet(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x21), common.Address{}, 5000)
	b := seedWalletAt(t, s, "b", addr(0x22), common.Address{}, 5000)

	done := newPending(a.ID, 4000, "")
	update(t, s, func(tx *Tx) error {
		if err := tx.PutPending(newPending(a.ID, 100, "")); err != nil {
			return err
		}
		if err := tx.PutPending(newPending(a.ID, 250, "")); err != nil {
			return err
		}
		if err := tx.PutPending(newPending(b.ID, 900, "")); err != nil {
			return err
		}
		if err := tx.PutPending(done); err != nil {
			return err
		}
		_, err := tx.Settle(done.ID, hash(0x77), 10, time.Now().UTC())
		return err
	})

	if err := s.View(func(tx *Tx) error {
		got, err := tx.Committed(a.ID)
		if err != nil {
			return err
		}
		if got.Int64() != 350 {
			t.Fatalf("committed(a) = %s, want 350 — the settled 4000 must not count", got)
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

func TestMutatePendingRefusesImmutableFields(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	p := newPending(w.ID, 100, "k")
	update(t, s, func(tx *Tx) error { return tx.PutPending(p) })

	cases := map[string]func(*Pending){
		"id":              func(x *Pending) { x.ID = uuid.New() },
		"wallet":          func(x *Pending) { x.Wallet = nextID() },
		"destination":     func(x *Pending) { x.Destination = addr(0x11) },
		"amount":          func(x *Pending) { x.Amount = wei(999) },
		"idempotency key": func(x *Pending) { x.IdempotencyKey = "other" },
		"created_at":      func(x *Pending) { x.CreatedAt = time.Now().Add(time.Hour) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.Update(func(tx *Tx) error {
				_, err := tx.MutatePending(p.ID, func(x *Pending) error {
					mutate(x)
					return nil
				})
				return err
			})
			if !errors.Is(err, ErrImmutable) {
				t.Fatalf("got %v, want ErrImmutable", err)
			}
		})
	}

	// What may change is how the attempt is going.
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutatePending(p.ID, func(x *Pending) error {
			x.Attempts++
			x.Error = "reverted"
			return nil
		})
		return err
	})
}

func TestWalletSettledIsListedMostRecentFirst(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 10_000)
	for i := range 3 {
		p := newPending(w.ID, int64(i+1), "")
		p.CreatedAt = time.Now().UTC().Add(time.Duration(i) * time.Second)
		update(t, s, func(tx *Tx) error {
			if err := tx.PutPending(p); err != nil {
				return err
			}
			_, err := tx.Settle(p.ID, hash(byte(i+1)), uint64(i+1), time.Now().UTC())
			return err
		})
	}

	if err := s.View(func(tx *Tx) error {
		list, err := tx.WalletSettled(w.ID, 0)
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

func TestMutateMissingPending(t *testing.T) {
	s := open(t)
	err := s.Update(func(tx *Tx) error {
		_, err := tx.MutatePending(uuid.New(), func(*Pending) error { return nil })
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// Go's zero value is a valid uint8, so a construction site that forgets the
// reason compiles silently and writes a debit that explains nothing. The store
// is the only place that can catch it, and it catches it in both keyspaces (§42).
func TestDebitsMustNameAReason(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)

	err := s.Update(func(tx *Tx) error {
		return tx.PutPending(Pending{
			ID: uuid.New(), Wallet: w.ID, // Reason omitted
			Destination: addr(0x99), Amount: wei(10), CreatedAt: time.Now().UTC(),
		})
	})
	if err == nil || !strings.Contains(err.Error(), "no reason") {
		t.Fatalf("pending: err = %v, want it to name the missing reason", err)
	}

	err = s.Update(func(tx *Tx) error {
		return tx.PutWithdrawal(Withdrawal{
			ID: uuid.New(), Wallet: w.ID, // Reason omitted
			Destination: addr(0x99), Amount: wei(10), TxHash: hash(1),
			CreatedAt: time.Now().UTC(),
		})
	})
	if err == nil || !strings.Contains(err.Error(), "no reason") {
		t.Fatalf("settled: err = %v, want it to name the missing reason", err)
	}
}

// Being in the log means it happened, so it has to say what happened.
func TestASettledDebitMustNameItsTransaction(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "hot", 1000)
	err := s.Update(func(tx *Tx) error {
		return tx.PutWithdrawal(Withdrawal{
			ID: uuid.New(), Wallet: w.ID, Reason: ReasonDrain,
			Destination: addr(0x99), Amount: wei(10), CreatedAt: time.Now().UTC(),
		})
	})
	if err == nil || !strings.Contains(err.Error(), "no transaction") {
		t.Fatalf("err = %v, want it to refuse a settled debit with no tx", err)
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
			Destination: addr(0x99), Amount: wei(10), TxHash: hash(3),
			CreatedAt: time.Now().UTC(),
		}
		return put(tx, bWithdrawal, wd.ID[:], wd.encode)
	})

	found := findingsOfKind(verify(t, s), "ownership")
	if len(found) != 1 {
		t.Fatalf("ownership findings = %d, want 1 (%v)", len(found), verify(t, s).Findings)
	}
	if !strings.Contains(found[0].Detail, "reason 99") {
		t.Fatalf("detail = %q, want it to name the value", found[0].Detail)
	}
}

// A drain is recorded once it has already happened, so one waiting as a promise
// is something nobody will ever keep (§42).
func TestAuditFlagsAPendingDrain(t *testing.T) {
	s := open(t)
	hot := seedWallet(t, s, "hot", 0)
	p := seedProxy(t, s, "cust-1", hot.Address, 500)

	update(t, s, func(tx *Tx) error {
		return tx.PutPending(Pending{
			ID: uuid.New(), Wallet: p.ID, Reason: ReasonDrain,
			Destination: hot.Address, Amount: wei(500), CreatedAt: time.Now().UTC(),
		})
	})

	found := findingsOfKind(verify(t, s), "ownership")
	if len(found) != 1 || !strings.Contains(found[0].Detail, "drain is pending") {
		t.Fatalf("findings = %v, want one naming the pending drain", verify(t, s).Findings)
	}
}
