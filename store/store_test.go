package store

import (
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

// seedWallet creates an accumulating wallet holding `balance`, which is the
// shape most other tests need before they can do anything interesting.
//
// The balance is seeded the way a real one arrives — a recorded deposit —
// rather than by writing a number onto the wallet. Verify walks the deposit
// records, so a balance with nothing behind it starts every test dirty.
func seedWallet(t *testing.T, s *Store, ref string, balance int64) Wallet {
	t.Helper()
	return seedWalletAt(t, s, ref, addr(0x01), common.Address{}, balance)
}

// seedProxy creates a forwarding wallet: one whose deposits are owed onward to
// drainTo rather than payable from it.
func seedProxy(t *testing.T, s *Store, ref string, drainTo common.Address, balance int64) Wallet {
	t.Helper()
	return seedWalletAt(t, s, ref, addr(0x02), drainTo, balance)
}

func seedWalletAt(t *testing.T, s *Store, ref string, at, drainTo common.Address, balance int64) Wallet {
	t.Helper()
	w := Wallet{
		ID: uuid.New(), Ref: ref, Kind: KindManaged, Address: at, DrainTo: drainTo,
		Active: true, Balance: wei(balance),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	update(t, s, func(tx *Tx) error {
		if err := tx.PutWallet(w); err != nil {
			return err
		}
		if balance == 0 {
			return nil
		}
		// (block, log_index) is globally unique on a chain — a log index counts
		// across every receipt in the block — so the cursor index needs no
		// scoping. Seeded deposits have to respect that or they collide in a
		// way a real chain never would.
		_, err := tx.PutDeposit(Deposit{
			Wallet: w.ID, Block: 1, LogIndex: uint32(at[common.AddressLength-1]),
			TxHash: hash(at[common.AddressLength-1]),
			From:   addr(0xF0), Amount: wei(balance),
			Status: DepositReceived, CreatedAt: time.Now().UTC(),
		})
		return err
	})
	return w
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
	top := seedWallet(t, s, "hot", 500)

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
	top := seedWallet(t, s, "hot", 1234)

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

// TestMetaRefusesADifferentChain: every wallet in a database was derived for
// one chain and every amount is denominated by one token. Moving the file
// between them is a migration, and the guard is what stops it happening by
// accident — a mistake that would otherwise show up as a service that reads
// blocks perfectly and can sign nothing.
func TestMetaRefusesADifferentChain(t *testing.T) {
	s := open(t)
	mainnet := Meta{ChainID: 56, Token: addr(0x55), Decimals: 18}
	update(t, s, func(tx *Tx) error { return tx.SetMeta(mainnet) })

	// Re-recording the same identity is how every normal restart goes.
	update(t, s, func(tx *Tx) error { return tx.SetMeta(mainnet) })

	for name, m := range map[string]Meta{
		"another chain":    {ChainID: 97, Token: addr(0x55), Decimals: 18},
		"another token":    {ChainID: 56, Token: addr(0x99), Decimals: 18},
		"another decimals": {ChainID: 56, Token: addr(0x55), Decimals: 6},
	} {
		err := s.Update(func(tx *Tx) error { return tx.SetMeta(m) })
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}

	got, ok, err := readMeta(t, s)
	if err != nil || !ok || got != mainnet {
		t.Fatalf("meta = %+v (ok=%v err=%v), want it unchanged", got, ok, err)
	}
}

func readMeta(t *testing.T, s *Store) (Meta, bool, error) {
	t.Helper()
	var (
		m   Meta
		ok  bool
		err error
	)
	if verr := s.View(func(tx *Tx) error {
		m, ok, err = tx.Meta()
		return nil
	}); verr != nil {
		t.Fatalf("View: %v", verr)
	}
	return m, ok, err
}
