package cli

import (
	"context"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bsc/config"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func storeWith(t *testing.T, balance int64) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	w := store.Wallet{
		ID: uuid.New(), Ref: "hot", Kind: store.KindManaged, Address: common.HexToAddress("0xabc"),
		Balance: big.NewInt(balance), CreatedAt: time.Now(),
	}
	if err := st.Update(func(tx *store.Tx) error {
		if err := tx.PutWallet(w); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st
}

func snapshots(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestSnapshotIsAUsableDatabase(t *testing.T) {
	st := storeWith(t, 4242)
	dir := t.TempDir()

	if err := snapshot(st, dir, 10, quietLog()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	files := snapshots(t, dir)
	if len(files) != 1 || !strings.HasPrefix(files[0], snapshotPrefix) {
		t.Fatalf("files = %v", files)
	}
	// No half-written file left behind: a truncated backup that looks usable is
	// worse than no backup.
	for _, f := range files {
		if strings.HasSuffix(f, ".partial") {
			t.Fatalf("partial file survived: %s", f)
		}
	}

	// The whole point: it opens and audits as a database in its own right.
	rep, err := Verify(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() || rep.Wallets != 1 {
		t.Fatalf("snapshot audit = %+v", rep)
	}
}

func TestSnapshotPruningKeepsTheNewest(t *testing.T) {
	st := storeWith(t, 1)
	dir := t.TempDir()

	// Names carry a second-resolution timestamp, so write distinct ones directly
	// rather than sleeping through real time.
	for _, name := range []string{
		snapshotPrefix + "20260101T000000Z.db",
		snapshotPrefix + "20260102T000000Z.db",
		snapshotPrefix + "20260103T000000Z.db",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// Something else living in the directory must survive.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := prune(dir, 2, quietLog()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got := snapshots(t, dir)
	if len(got) != 3 { // two snapshots plus the unrelated file
		t.Fatalf("files = %v", got)
	}
	for _, name := range got {
		if strings.Contains(name, "20260101") {
			t.Fatalf("pruning kept the oldest: %v", got)
		}
	}
	var keptOther bool
	for _, name := range got {
		if name == "notes.txt" {
			keptOther = true
		}
	}
	if !keptOther {
		t.Fatalf("pruning deleted an unrelated file: %v", got)
	}

	_ = st
}

func TestPruningIsDisabledByAZeroKeep(t *testing.T) {
	dir := t.TempDir()
	name := snapshotPrefix + "20260101T000000Z.db"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := prune(dir, 0, quietLog()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(snapshots(t, dir)) != 1 {
		t.Fatal("a zero keep deleted snapshots")
	}
}

func TestSnapshotsDisabledWithoutADirectory(t *testing.T) {
	st := storeWith(t, 1)
	if err := runSnapshots(context.Background(), st, config.Config{}, quietLog()); err != nil {
		t.Fatalf("runSnapshots: %v", err)
	}
}

func TestRunSnapshotsWritesAndStops(t *testing.T) {
	st := storeWith(t, 7)
	dir := filepath.Join(t.TempDir(), "backups") // not yet created
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runSnapshots(ctx, st, config.Config{
			SnapshotDir: dir, SnapshotInterval: 5 * time.Millisecond, SnapshotKeep: 2,
		}, quietLog())
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no snapshot was written")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSnapshots returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSnapshots did not stop")
	}
}
