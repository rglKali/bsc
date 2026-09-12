package store

import (
	"testing"
	"time"

	"bsc/money"
)

// putDeposit records a deposit for w at (block, logIndex) and asserts creation.
func putDeposit(t *testing.T, s *Store, w Wallet, block uint64, logIndex uint32, txb byte, amount int64) Deposit {
	t.Helper()
	d := Deposit{
		Wallet: w.ID, App: w.App, Block: block, LogIndex: logIndex, TxHash: hash(txb),
		From: addr(0xF0), AmountWei: wei(amount), Cents: money.Cents(amount), Status: DepositConfirmed, CreatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error {
		created, err := tx.PutDeposit(d)
		if err != nil {
			return err
		}
		if !created {
			t.Fatalf("deposit %d-%d already existed", block, logIndex)
		}
		return nil
	})
	return d
}

func TestDepositDedupIsAPropertyOfTheKey(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x50, 0)
	d := putDeposit(t, s, w, 100, 0, 0x01, 5)

	update(t, s, func(tx *Tx) error {
		created, err := tx.PutDeposit(d)
		if err != nil {
			return err
		}
		if created {
			t.Fatal("re-recording the same transfer reported creation")
		}
		return nil
	})

	// A reprocessed block must not duplicate the feed either.
	if err := s.View(func(tx *Tx) error {
		got, _, err := tx.DepositsSince("df", Cursor{}, 0)
		if err != nil {
			return err
		}
		if len(got) != 1 {
			t.Fatalf("feed has %d entries after a replay, want 1", len(got))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestTwoTransfersInOneTransactionAreDistinctDeposits(t *testing.T) {
	// Same tx hash, different log index: the exact case that motivated keying
	// deposits on (hash, log_index) rather than the hash alone.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x51, 0)

	putDeposit(t, s, w, 100, 3, 0x02, 5)
	putDeposit(t, s, w, 100, 7, 0x02, 6)

	if err := s.View(func(tx *Tx) error {
		got, next, err := tx.DepositsSince("df", Cursor{}, 0)
		if err != nil {
			return err
		}
		if len(got) != 2 {
			t.Fatalf("got %d deposits, want 2", len(got))
		}
		if got[0].LogIndex != 3 || got[1].LogIndex != 7 {
			t.Fatalf("log indexes = %d,%d; want 3,7 in order", got[0].LogIndex, got[1].LogIndex)
		}
		if next != (Cursor{Block: 100, LogIndex: 7}) {
			t.Fatalf("next cursor = %v", next)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestDepositFeedIsOrderedAndPaginates(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x52, 0)

	// Insert out of order to prove the index, not the insertion sequence, is
	// what orders the feed.
	putDeposit(t, s, w, 300, 1, 0x13, 3)
	putDeposit(t, s, w, 100, 5, 0x11, 1)
	putDeposit(t, s, w, 200, 0, 0x12, 2)

	if err := s.View(func(tx *Tx) error {
		all, _, err := tx.DepositsSince("df", Cursor{}, 0)
		if err != nil {
			return err
		}
		wantBlocks := []uint64{100, 200, 300}
		for i, d := range all {
			if d.Block != wantBlocks[i] {
				t.Fatalf("feed order = %v, want %v", blocks(all), wantBlocks)
			}
		}

		// Page through one at a time, carrying the cursor.
		var seen []uint64
		cur := Cursor{}
		for {
			page, next, err := tx.DepositsSince("df", cur, 1)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].Block)
			if next == cur {
				t.Fatalf("cursor did not advance past %v", cur)
			}
			cur = next
		}
		if len(seen) != 3 || seen[0] != 100 || seen[2] != 300 {
			t.Fatalf("paged blocks = %v", seen)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestDepositCursorIsStableWhenNothingIsNew(t *testing.T) {
	// An app that keeps passing its cursor back must never lose its place.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x53, 0)
	putDeposit(t, s, w, 100, 0, 0x21, 1)

	if err := s.View(func(tx *Tx) error {
		_, next, err := tx.DepositsSince("df", Cursor{}, 0)
		if err != nil {
			return err
		}
		got, again, err := tx.DepositsSince("df", next, 0)
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Fatalf("got %d deposits past the end", len(got))
		}
		if again != next {
			t.Fatalf("cursor moved on an empty read: %v -> %v", next, again)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestDepositFeedIsPerApp(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	seedApp(t, s, "lkr:acme", 0)
	a := depositWallet(t, s, "df", "cust-1", 0x54, 0)
	b := depositWallet(t, s, "lkr:acme", "cust-1", 0x55, 0)
	putDeposit(t, s, a, 100, 0, 0x31, 1)
	putDeposit(t, s, b, 101, 0, 0x32, 2)

	if err := s.View(func(tx *Tx) error {
		for _, tc := range []struct {
			slug  string
			block uint64
		}{{"df", 100}, {"lkr:acme", 101}} {
			got, _, err := tx.DepositsSince(tc.slug, Cursor{}, 0)
			if err != nil {
				return err
			}
			if len(got) != 1 || got[0].Block != tc.block {
				t.Fatalf("%s feed = %v, want just block %d", tc.slug, blocks(got), tc.block)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestOneSweepCreditsEveryOpenDepositOnTheWallet(t *testing.T) {
	// A drain moves the whole balance, so one sweep credits several deposits at
	// once — which is precisely why crediting is a status on the record and not
	// an entry in the feed keyed on the sweep's log position.
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x56, 0)
	other := depositWallet(t, s, "df", "cust-2", 0x57, 0)

	putDeposit(t, s, w, 100, 0, 0x41, 1)
	putDeposit(t, s, w, 101, 2, 0x42, 2)
	putDeposit(t, s, other, 102, 0, 0x43, 3)

	sweep := hash(0xAA)
	update(t, s, func(tx *Tx) error {
		n, cents, err := tx.CreditDeposits("df", w.ID, sweep)
		if err != nil {
			return err
		}
		if n != 2 {
			t.Fatalf("credited %d deposits, want 2", n)
		}
		// The cents are what the caller must add to the app's ledger: one
		// sweep, both deposits, one credit (§22).
		if cents != 3 {
			t.Fatalf("credited %d cents, want 1+2", cents)
		}
		return nil
	})

	if err := s.View(func(tx *Tx) error {
		all, _, err := tx.DepositsSince("df", Cursor{}, 0)
		if err != nil {
			return err
		}
		for _, d := range all {
			credited := d.Wallet == w.ID
			if credited && (d.Status != DepositCredited || d.DrainTx != sweep) {
				t.Fatalf("deposit %d-%d not credited: %s %s", d.Block, d.LogIndex, d.Status, d.DrainTx.Hex())
			}
			if !credited && d.Status != DepositConfirmed {
				t.Fatalf("other wallet's deposit was touched: %s", d.Status)
			}
		}
		// Only the untouched wallet's deposit should remain open.
		open, err := tx.OpenDeposits("df", 0)
		if err != nil {
			return err
		}
		if len(open) != 1 || open[0].Wallet != other.ID {
			t.Fatalf("open set = %d entries, want just cust-2's", len(open))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestCreditDepositsIsANoOpWhenNothingIsOpen(t *testing.T) {
	s := open(t)
	seedApp(t, s, "df", 0)
	w := depositWallet(t, s, "df", "cust-1", 0x58, 0)

	update(t, s, func(tx *Tx) error {
		n, cents, err := tx.CreditDeposits("df", w.ID, hash(1))
		if err != nil {
			return err
		}
		if n != 0 || cents != 0 {
			t.Fatalf("credited %d deposits / %d cents, want nothing", n, cents)
		}
		return nil
	})
}

func blocks(ds []Deposit) []uint64 {
	out := make([]uint64, len(ds))
	for i, d := range ds {
		out[i] = d.Block
	}
	return out
}
