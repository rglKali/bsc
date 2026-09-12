package store

import (
	"bsc/money"

	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// open returns a store backed by a fresh file in the test's temp dir.
func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// update runs fn in a write transaction and fails the test on error.
func update(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	if err := s.Update(fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func wei(v int64) *big.Int { return big.NewInt(v) }

func addr(b byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = b
	return a
}

func hash(b byte) common.Hash {
	var h common.Hash
	h[common.HashLength-1] = b
	return h
}

// seedApp creates an app holding `balance`, the shape every other test needs
// before it can do anything interesting.
//
// The balance is seeded the way a real one arrives — a credited deposit — rather
// than by writing a number onto the app. That is not ceremony: the ledger is
// recomputed from the log by Verify, so a balance with no deposit behind it is
// exactly the drift the audit exists to catch, and every test would start dirty
// (§22).
//
// Tests count in whole cents and their wallets hold the same figure in wei,
// which is the truth when a token has two decimals. Where the distinction
// matters, the test says so explicitly.
func seedApp(t *testing.T, s *Store, slug string, balance int64) (App, Wallet) {
	t.Helper()
	top := Wallet{
		ID: uuid.New(), App: slug, Kind: KindTopLevel, Address: addr(0x01),
		Active: true, Balance: wei(balance), CreatedAt: time.Now().UTC(),
	}
	app := App{
		Slug: slug, Wallet: top.ID,
		Fee:       FeePolicy{Flat: 100},
		Ledger:    money.Cents(balance),
		CreatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error {
		if err := tx.PutWallet(top); err != nil {
			return err
		}
		if err := tx.PutApp(app); err != nil {
			return err
		}
		if balance == 0 {
			return nil
		}
		// The deposit that justifies the ledger, already swept.
		src := Wallet{
			ID: uuid.New(), App: slug, Kind: KindDeposit, Ref: "seed", Address: addr(0x02),
			Balance: new(big.Int), CreatedAt: time.Now().UTC(),
		}
		if err := tx.PutWallet(src); err != nil {
			return err
		}
		_, err := tx.PutDeposit(Deposit{
			Wallet: src.ID, App: slug, Block: 1, LogIndex: 0, TxHash: hash(0xEE),
			From: addr(0xF0), AmountWei: wei(balance), Cents: money.Cents(balance),
			Status: DepositCredited, DrainTx: hash(0xED), CreatedAt: time.Now().UTC(),
		})
		return err
	})
	return app, top
}

func TestOpenIsIdempotentAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bsc.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	update(t, s, func(tx *Tx) error { return tx.SetCursor(4242) })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.View(func(tx *Tx) error {
		next, ok, err := tx.Cursor()
		if err != nil {
			return err
		}
		if !ok || next != 4242 {
			t.Fatalf("cursor = (%d, %v), want (4242, true)", next, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestCursorAbsentOnFreshDatabase(t *testing.T) {
	// A fresh database must report "no cursor" rather than block 0: the caller
	// has to fall back to the finalized head, because starting at 0 would try
	// to scan from genesis.
	s := open(t)
	if err := s.View(func(tx *Tx) error {
		next, ok, err := tx.Cursor()
		if err != nil {
			return err
		}
		if ok || next != 0 {
			t.Fatalf("fresh cursor = (%d, %v), want (0, false)", next, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestOpenRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bsc.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Forge a database written by a newer binary.
	update(t, s, func(tx *Tx) error {
		return tx.tx.Bucket(bMeta).Put(keyVersion, be64(schemaVersion+1))
	})
	s.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a newer schema version; want refusal")
	}
}

func TestSnapshotIsReadable(t *testing.T) {
	s := open(t)
	_, top := seedApp(t, s, "df", 500)

	var buf bytes.Buffer
	n, err := s.Snapshot(&buf)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if n == 0 || buf.Len() == 0 {
		t.Fatalf("Snapshot wrote %d bytes", n)
	}

	// The snapshot must open as a database in its own right — that is the whole
	// point: inspection happens on copies, since bbolt holds the live file.
	path := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer s2.Close()
	if err := s2.View(func(tx *Tx) error {
		got, ok, err := tx.Wallet(top.ID)
		if err != nil || !ok {
			t.Fatalf("wallet in snapshot: ok=%v err=%v", ok, err)
		}
		if got.Balance.Cmp(wei(500)) != 0 {
			t.Fatalf("snapshot balance = %s, want 500", got.Balance)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

func TestOpenReadOnlyReadsASnapshot(t *testing.T) {
	// The live database is exclusively locked by the running service, which is
	// exactly why inspection happens on snapshots.
	s := open(t)
	_, top := seedApp(t, s, "df", 1234)

	path := filepath.Join(t.TempDir(), "snap.db")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Snapshot(file); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	file.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	if err := ro.View(func(tx *Tx) error {
		w, ok, err := tx.Wallet(top.ID)
		if err != nil || !ok {
			t.Fatalf("wallet: ok=%v err=%v", ok, err)
		}
		if w.Balance.Cmp(wei(1234)) != 0 {
			t.Fatalf("balance = %s", w.Balance)
		}
		rep, err := tx.Verify()
		if err != nil {
			return err
		}
		if !rep.OK() {
			t.Fatalf("snapshot audit: %v", rep.Findings)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	// Writes must be refused rather than silently dropped.
	if err := ro.Update(func(tx *Tx) error { return tx.SetCursor(1) }); err == nil {
		t.Fatal("a read-only store accepted a write")
	}
}
