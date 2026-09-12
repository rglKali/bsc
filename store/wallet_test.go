package store

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// depositWallet creates a deposit wallet for slug with the given ref.
func depositWallet(t *testing.T, s *Store, slug, ref string, a byte, balance int64) Wallet {
	t.Helper()
	w := Wallet{
		ID: uuid.New(), App: slug, Kind: KindDeposit, Ref: ref, Address: addr(a),
		Balance: wei(balance), CreatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error { return tx.PutWallet(w) })
	return w
}

func TestWalletLookupIndexes(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x20, 0)

	if err := s.View(func(tx *Tx) error {
		byAddr, ok, err := tx.WalletByAddress(w.Address)
		if err != nil || !ok {
			t.Fatalf("WalletByAddress: ok=%v err=%v", ok, err)
		}
		if byAddr.ID != w.ID {
			t.Fatalf("WalletByAddress = %s, want %s", byAddr.ID, w.ID)
		}
		byRef, ok, err := tx.WalletByRef("df", "cust-1")
		if err != nil || !ok {
			t.Fatalf("WalletByRef: ok=%v err=%v", ok, err)
		}
		if byRef.ID != w.ID {
			t.Fatalf("WalletByRef = %s, want %s", byRef.ID, w.ID)
		}
		// A ref belongs to one app only.
		if _, ok, err := tx.WalletByRef("other", "cust-1"); err != nil || ok {
			t.Fatalf("ref leaked across apps: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestPutWalletValidates(t *testing.T) {
	s := open(t)
	base := Wallet{ID: uuid.New(), App: "df", Kind: KindDeposit, Ref: "r", Address: addr(1)}

	tests := map[string]func(Wallet) Wallet{
		"bad slug":       func(w Wallet) Wallet { w.App = "Bad Slug"; return w },
		"no id":          func(w Wallet) Wallet { w.ID = uuid.Nil; return w },
		"no address":     func(w Wallet) Wallet { w.Address = addr(0); return w },
		"deposit no ref": func(w Wallet) Wallet { w.Ref = ""; return w },
	}
	for name, mutate := range tests {
		if err := s.Update(func(tx *Tx) error { return tx.PutWallet(mutate(base)) }); err == nil {
			t.Fatalf("%s: PutWallet accepted", name)
		}
	}
}

func TestMutateWalletRefusesIndexedFields(t *testing.T) {
	// These fields are what the indexes point at; letting one change would
	// silently orphan an index, which is the standing risk of hand-rolling them.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x21, 0)

	tests := map[string]func(*Wallet){
		"id":      func(w *Wallet) { w.ID = uuid.New() },
		"app":     func(w *Wallet) { w.App = "other" },
		"kind":    func(w *Wallet) { w.Kind = KindTopLevel },
		"ref":     func(w *Wallet) { w.Ref = "cust-2" },
		"address": func(w *Wallet) { w.Address = addr(0x99) },
	}
	for name, mutate := range tests {
		err := s.Update(func(tx *Tx) error {
			_, err := tx.MutateWallet(w.ID, func(w *Wallet) error { mutate(w); return nil })
			return err
		})
		if !errors.Is(err, ErrImmutable) {
			t.Fatalf("%s: got %v, want ErrImmutable", name, err)
		}
	}
}

func TestCreditAndDebit(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x22, 0)

	update(t, s, func(tx *Tx) error {
		if _, err := tx.Credit(w.ID, wei(100)); err != nil {
			return err
		}
		if _, err := tx.Credit(w.ID, wei(50)); err != nil {
			return err
		}
		got, underflow, err := tx.Debit(w.ID, wei(30))
		if err != nil {
			return err
		}
		if underflow {
			t.Fatal("unexpected underflow")
		}
		if got.Balance.Cmp(wei(120)) != 0 {
			t.Fatalf("balance = %s, want 120", got.Balance)
		}
		return nil
	})
}

func TestDebitUnderflowClampsAndReports(t *testing.T) {
	// Underflow can only mean a bug in our own accounting. Aborting the block
	// would wedge chain sync on a condition retrying cannot fix, so the store
	// clamps and tells the caller, who logs loudly.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x23, 10)

	update(t, s, func(tx *Tx) error {
		got, underflow, err := tx.Debit(w.ID, wei(25))
		if err != nil {
			return err
		}
		if !underflow {
			t.Fatal("Debit did not report underflow")
		}
		if got.Balance.Sign() != 0 {
			t.Fatalf("balance = %s, want clamped to 0", got.Balance)
		}
		return nil
	})
}

func TestReserveRefusesOverdraft(t *testing.T) {
	// v1 had no balance check at all: an oversized withdrawal was only
	// discovered when the transfer reverted, after the gas was spent.
	s := open(t)
	seedApp(t, s, "df", 100)

	update(t, s, func(tx *Tx) error {
		if _, err := tx.ReserveLedger("df", 60); err != nil {
			return err
		}
		// 60 of 100 held; 50 more must not fit.
		_, err := tx.ReserveLedger("df", 50)
		if !errors.Is(err, ErrInsufficient) {
			t.Fatalf("second ReserveLedger = %v, want ErrInsufficient", err)
		}
		a, _, err := tx.App("df")
		if err != nil {
			return err
		}
		if a.Reserved != 60 {
			t.Fatalf("reserved = %d, want 60 (the refused reservation must not apply)", a.Reserved)
		}
		if a.Spendable() != 40 {
			t.Fatalf("spendable = %d, want 40", a.Spendable())
		}
		return nil
	})
}

func TestReleaseIsExactAndClamps(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 100)

	update(t, s, func(tx *Tx) error {
		if _, err := tx.ReserveLedger("df", 40); err != nil {
			return err
		}
		a, err := tx.ReleaseLedger("df", 40)
		if err != nil {
			return err
		}
		if a.Reserved != 0 {
			t.Fatalf("reserved = %d, want 0", a.Reserved)
		}
		// Over-releasing should never happen (callers release the recorded
		// figure) but must not produce a negative reservation if it does.
		a, err = tx.ReleaseLedger("df", 10)
		if err != nil {
			return err
		}
		if a.Reserved != 0 {
			t.Fatalf("reserved = %d after over-release, want 0", a.Reserved)
		}
		return nil
	})
}

// TestDebitRefusesToOverdrawTheLedger guards the books directly: a debit larger
// than the ledger can only be our own bug, and clamping it would hide the bug
// while leaving the app credited for money that left.
func TestDebitRefusesToOverdrawTheLedger(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 100)
	update(t, s, func(tx *Tx) error {
		if _, err := tx.DebitLedger("df", 101); !errors.Is(err, ErrInsufficient) {
			t.Fatalf("DebitLedger past the ledger = %v, want ErrInsufficient", err)
		}
		a, _, err := tx.App("df")
		if err != nil {
			return err
		}
		if a.Ledger != 100 {
			t.Fatalf("ledger = %d after a refused debit, want 100", a.Ledger)
		}
		return nil
	})
}

func TestSpendableNeverGoesNegative(t *testing.T) {
	a := App{Ledger: 5, Reserved: 9}
	if got := a.Spendable(); got != 0 {
		t.Fatalf("Spendable = %d, want 0", got)
	}
}

func TestClaimWalletIsExclusive(t *testing.T) {
	// This is the enforcement point for "at most one live flow per wallet" —
	// the invariant that stops a second deposit re-triggering activation.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x24, 0)
	first, second := uuid.New(), uuid.New()

	update(t, s, func(tx *Tx) error {
		if _, err := tx.ClaimWallet(w.ID, first); err != nil {
			return err
		}
		// Re-claiming by the same flow is idempotent...
		if _, err := tx.ClaimWallet(w.ID, first); err != nil {
			t.Fatalf("re-claim by the owner: %v", err)
		}
		// ...but a different flow must be refused.
		if _, err := tx.ClaimWallet(w.ID, second); err == nil {
			t.Fatal("second flow claimed an owned wallet")
		}
		if _, err := tx.ReleaseWallet(w.ID); err != nil {
			return err
		}
		if _, err := tx.ClaimWallet(w.ID, second); err != nil {
			t.Fatalf("claim after release: %v", err)
		}
		return nil
	})
}

func TestBackoff(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x25, 0)
	until := time.Now().Add(time.Minute).UTC().Truncate(time.Nanosecond)

	update(t, s, func(tx *Tx) error {
		got, err := tx.BackOff(w.ID, until)
		if err != nil {
			return err
		}
		if got.FailedAttempts != 1 || !got.RetryAfter.Equal(until) {
			t.Fatalf("backoff = (%d, %v), want (1, %v)", got.FailedAttempts, got.RetryAfter, until)
		}
		if got, err = tx.BackOff(w.ID, until); err != nil {
			return err
		}
		if got.FailedAttempts != 2 {
			t.Fatalf("attempts = %d, want 2", got.FailedAttempts)
		}
		if got, err = tx.ClearBackoff(w.ID); err != nil {
			return err
		}
		if got.FailedAttempts != 0 || !got.RetryAfter.IsZero() {
			t.Fatalf("cleared backoff = (%d, %v)", got.FailedAttempts, got.RetryAfter)
		}
		return nil
	})
}

func TestDepositWalletsPaginateByRef(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	for i, ref := range []string{"a", "b", "c", "d"} {
		depositWallet(t, s, "df", ref, byte(0x30+i), 0)
	}
	// Another app's wallets must not appear.
	seedApp(t, s, "other", 0)
	depositWallet(t, s, "other", "a", 0x40, 0)

	if err := s.View(func(tx *Tx) error {
		page, err := tx.DepositWallets("df", "", 2)
		if err != nil {
			return err
		}
		if len(page) != 2 || page[0].Ref != "a" || page[1].Ref != "b" {
			t.Fatalf("first page = %v", refs(page))
		}
		next, err := tx.DepositWallets("df", page[len(page)-1].Ref, 2)
		if err != nil {
			return err
		}
		if len(next) != 2 || next[0].Ref != "c" || next[1].Ref != "d" {
			t.Fatalf("second page = %v", refs(next))
		}
		last, err := tx.DepositWallets("df", "d", 2)
		if err != nil {
			return err
		}
		if len(last) != 0 {
			t.Fatalf("page past the end = %v", refs(last))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func refs(ws []Wallet) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Ref
	}
	return out
}
