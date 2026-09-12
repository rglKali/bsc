package store

import (
	"bsc/money"

	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newWithdrawal builds a queued withdrawal reserving its payout.
func newWithdrawal(slug string, payout, fee int64, key string) Withdrawal {
	return Withdrawal{
		ID: uuid.New(), App: slug, Destination: addr(0xD0),
		Amount: money.Cents(payout), Fee: money.Cents(fee),
		Payout: money.Cents(payout), Debit: money.Cents(payout + fee),
		Status: WithdrawalQueued, IdempotencyKey: key,
		FeeSnapshot: FeePolicy{Flat: money.Cents(fee)},
		CreatedAt:   time.Now().UTC(),
	}
}

func TestWithdrawalIdempotencyLookup(t *testing.T) {
	// Over HTTP a client retry after a timeout would otherwise pay twice; v1
	// got this free from the broker's message-id dedup.
	s := open(t)
	seedApp(t, s, "df", 1000)
	wd := newWithdrawal("df", 10, 1, "k-1")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.WithdrawalByKey("df", "k-1")
		if err != nil || !ok {
			t.Fatalf("WithdrawalByKey: ok=%v err=%v", ok, err)
		}
		if got.ID != wd.ID {
			t.Fatalf("resolved %s, want %s", got.ID, wd.ID)
		}
		// Keys are scoped per app.
		if _, ok, err := tx.WithdrawalByKey("other", "k-1"); err != nil || ok {
			t.Fatalf("key leaked across apps: ok=%v err=%v", ok, err)
		}
		if _, ok, err := tx.WithdrawalByKey("df", "k-2"); err != nil || ok {
			t.Fatalf("unknown key resolved: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestWithdrawalsWithoutAKeyAreStillStored(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 1000)
	wd := newWithdrawal("df", 10, 1, "")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	if err := s.View(func(tx *Tx) error {
		if _, ok, err := tx.Withdrawal(wd.ID); err != nil || !ok {
			t.Fatalf("Withdrawal: ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestOpenSetTracksTerminalStatus(t *testing.T) {
	// The open set *is* the withdrawal notification mechanism: an app polls it
	// and drops each entry as it settles.
	s := open(t)
	seedApp(t, s, "df", 1000)
	a := newWithdrawal("df", 10, 1, "k-a")
	b := newWithdrawal("df", 20, 1, "k-b")
	update(t, s, func(tx *Tx) error {
		if err := tx.PutWithdrawal(a); err != nil {
			return err
		}
		return tx.PutWithdrawal(b)
	})

	assertOpen := func(want int) {
		t.Helper()
		if err := s.View(func(tx *Tx) error {
			open, err := tx.OpenWithdrawals("df")
			if err != nil {
				return err
			}
			if len(open) != want {
				t.Fatalf("open set = %d, want %d", len(open), want)
			}
			return nil
		}); err != nil {
			t.Fatalf("View: %v", err)
		}
	}
	assertOpen(2)

	// pending is not terminal — it stays visible to the app.
	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWithdrawal(a.ID, func(wd *Withdrawal) error {
			wd.Status = WithdrawalPending
			wd.TxHash = hash(1)
			return nil
		})
		return err
	})
	assertOpen(2)

	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWithdrawal(a.ID, func(wd *Withdrawal) error {
			wd.Status = WithdrawalDone
			return nil
		})
		return err
	})
	assertOpen(1)

	update(t, s, func(tx *Tx) error {
		_, err := tx.MutateWithdrawal(b.ID, func(wd *Withdrawal) error {
			wd.Status = WithdrawalFailed
			wd.Error = "reverted"
			return nil
		})
		return err
	})
	assertOpen(0)

	// Terminal records remain readable by id — deposits and withdrawals are
	// kept forever, which is what removes the need for a replay window.
	if err := s.View(func(tx *Tx) error {
		got, ok, err := tx.Withdrawal(b.ID)
		if err != nil || !ok {
			t.Fatalf("terminal withdrawal unreadable: ok=%v err=%v", ok, err)
		}
		if got.Error != "reverted" {
			t.Fatalf("error = %q, want the failure reason preserved", got.Error)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestMutateWithdrawalRefusesImmutableFields(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 1000)
	wd := newWithdrawal("df", 10, 1, "k-1")
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })

	tests := map[string]func(*Withdrawal){
		"id":              func(w *Withdrawal) { w.ID = uuid.New() },
		"app":             func(w *Withdrawal) { w.App = "other" },
		"idempotency key": func(w *Withdrawal) { w.IdempotencyKey = "k-2" },
		"created_at":      func(w *Withdrawal) { w.CreatedAt = time.Now().Add(time.Hour) },
	}
	for name, mutate := range tests {
		err := s.Update(func(tx *Tx) error {
			_, err := tx.MutateWithdrawal(wd.ID, func(w *Withdrawal) error { mutate(w); return nil })
			return err
		})
		if !errors.Is(err, ErrImmutable) {
			t.Fatalf("%s: got %v, want ErrImmutable", name, err)
		}
	}
}

func TestWithdrawalsAreListedMostRecentFirst(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 1000)
	base := time.Now().UTC()
	for i := range 3 {
		wd := newWithdrawal("df", int64(10+i), 1, "")
		wd.CreatedAt = base.Add(time.Duration(i) * time.Second)
		wd.Amount = money.Cents(i) // marker
		update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(wd) })
	}
	// A second app's history must not bleed in.
	seedApp(t, s, "other", 1000)
	update(t, s, func(tx *Tx) error { return tx.PutWithdrawal(newWithdrawal("other", 99, 1, "")) })

	if err := s.View(func(tx *Tx) error {
		got, err := tx.Withdrawals("df", 0)
		if err != nil {
			return err
		}
		if len(got) != 3 {
			t.Fatalf("got %d withdrawals, want 3", len(got))
		}
		for i, wd := range got {
			want := money.Cents(2 - i) // newest first
			if wd.Amount != want {
				t.Fatalf("position %d = marker %d, want %d", i, wd.Amount, want)
			}
		}
		limited, err := tx.Withdrawals("df", 2)
		if err != nil {
			return err
		}
		if len(limited) != 2 || limited[0].Amount != 2 {
			t.Fatalf("limited page = %d entries starting at %d", len(limited), limited[0].Amount)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
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
