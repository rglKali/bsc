package store

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func putDeposit(t *testing.T, s *Store, w Wallet, block uint64, logIndex uint32, txb byte, amount int64) Deposit {
	t.Helper()
	d := Deposit{
		Wallet: w.ID, Block: block, LogIndex: logIndex, TxHash: hash(txb),
		From: addr(0xF0), Amount: wei(amount),
		CreatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error {
		_, err := tx.PutDeposit(d)
		return err
	})
	return d
}

// Dedup is a property of the key — tx hash ++ log index — so a reprocessed
// block writes nothing rather than needing a uniqueness check somebody could
// forget.
func TestDepositDedupIsAPropertyOfTheKey(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	d := putDeposit(t, s, w, 10, 0, 0xAA, 500)

	update(t, s, func(tx *Tx) error {
		created, err := tx.PutDeposit(d)
		if err != nil {
			return err
		}
		if created {
			t.Fatal("the same transfer was recorded twice")
		}
		return nil
	})
	if rep := verify(t, s); rep.Deposits != 1 {
		t.Fatalf("deposits = %d, want 1", rep.Deposits)
	}
}

func TestTwoTransfersInOneTransactionAreDistinctDeposits(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	putDeposit(t, s, w, 10, 0, 0xAA, 500)
	putDeposit(t, s, w, 10, 1, 0xAA, 700)

	rep := verify(t, s)
	mustBeClean(t, rep)
	if rep.Deposits != 2 {
		t.Fatalf("deposits = %d, want 2", rep.Deposits)
	}
}

func TestDepositFeedIsOrderedAndPaginates(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	// Written out of order on purpose: the index, not the write order, decides.
	putDeposit(t, s, w, 30, 0, 0xC3, 3)
	putDeposit(t, s, w, 10, 0, 0xC1, 1)
	putDeposit(t, s, w, 20, 0, 0xC2, 2)

	var page1, page2 []Deposit
	var cursor Cursor
	if err := s.View(func(tx *Tx) error {
		var err error
		if page1, cursor, err = tx.DepositsSince(Cursor{}, 2); err != nil {
			return err
		}
		page2, _, err = tx.DepositsSince(cursor, 2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := blocks(page1); len(got) != 2 || got[0] != 10 || got[1] != 20 {
		t.Fatalf("page 1 = %v, want [10 20]", got)
	}
	if got := blocks(page2); len(got) != 1 || got[0] != 30 {
		t.Fatalf("page 2 = %v, want [30]", got)
	}
}

// The feed is global now. Splitting it per app was a tenancy boundary bsc no
// longer draws (§37).
func TestDepositFeedSpansEveryWallet(t *testing.T) {
	s := open(t)
	a := seedWalletAt(t, s, "a", addr(0x21), common.Address{}, 0)
	b := seedWalletAt(t, s, "b", addr(0x22), common.Address{}, 0)
	putDeposit(t, s, a, 10, 0, 0xA1, 1)
	putDeposit(t, s, b, 11, 0, 0xB1, 2)

	if err := s.View(func(tx *Tx) error {
		all, _, err := tx.DepositsSince(Cursor{}, 0)
		if err != nil {
			return err
		}
		if len(all) != 2 {
			t.Fatalf("feed = %d deposits, want 2", len(all))
		}
		one, err := tx.WalletDeposits(a.ID, 0)
		if err != nil {
			return err
		}
		if len(one) != 1 || one[0].Wallet != a.ID {
			t.Fatalf("per-wallet view = %d deposits", len(one))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDepositCursorIsStableWhenNothingIsNew(t *testing.T) {
	s := open(t)
	w := seedWallet(t, s, "cust-1", 0)
	putDeposit(t, s, w, 10, 0, 0xAA, 500)

	if err := s.View(func(tx *Tx) error {
		_, first, err := tx.DepositsSince(Cursor{}, 0)
		if err != nil {
			return err
		}
		empty, second, err := tx.DepositsSince(first, 0)
		if err != nil {
			return err
		}
		if len(empty) != 0 {
			t.Fatalf("got %d deposits past the end", len(empty))
		}
		if second != first {
			t.Fatalf("cursor moved from %v to %v with nothing new", first, second)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func blocks(ds []Deposit) []uint64 {
	out := make([]uint64, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Block)
	}
	return out
}
