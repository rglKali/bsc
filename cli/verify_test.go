package cli

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// snapshotOf writes a database to a file and returns the path.
func snapshotOf(t *testing.T, st *store.Store) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snap.db")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if _, err := st.Snapshot(f); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return path
}

// seed builds a minimal consistent app with one wallet holding `balance`.
func seed(t *testing.T, balance int64) (*store.Store, store.Wallet) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	top := store.Wallet{
		ID: 1, Ref: "hot",
		Address: common.HexToAddress("0xabc"), Active: true,
		Balance: big.NewInt(balance), CreatedAt: time.Now(),
	}
	if err := st.Update(func(tx *store.Tx) error {
		return tx.PutWallet(top)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st, top
}

func TestVerifyReadsASnapshotOfALockedDatabase(t *testing.T) {
	// The live file stays open and locked throughout, which is the situation
	// this tool exists for.
	st, _ := seed(t, 100)
	path := snapshotOf(t, st)

	rep, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("clean database reported %v", rep.Findings)
	}
	if rep.Wallets != 1 {
		t.Fatalf("counts = %+v", rep)
	}
}

func TestVerifyReportsInconsistency(t *testing.T) {
	st, top := seed(t, 100)
	// A wallet promising more than it holds. With no ledger this is the
	// solvency question, asked of the thing that actually holds the money (§40).
	if err := st.Update(func(tx *store.Tx) error {
		return tx.PutPending(store.Pending{
			ID: uuid.New(), Wallet: top.ID, Reason: store.ReasonPayout,
			Destination: common.HexToAddress("0xdd"),
			Amount:      big.NewInt(500),
			CreatedAt:   time.Now(),
		})
	}); err != nil {
		t.Fatalf("seed withdrawal: %v", err)
	}
	rep, err := Verify(snapshotOf(t, st))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.OK() {
		t.Fatal("audit missed a wallet owing more than it holds")
	}
}

func TestVerifyOnChainCatchesBalanceDrift(t *testing.T) {
	// The half of the audit that needs the network. Custody — what a wallet
	// actually holds — cannot be recomputed from our own records at all, so only
	// the chain can answer it, and drift here means the watcher's view of a
	// transfer diverged from what the token did.
	st, top := seed(t, 100)
	path := snapshotOf(t, st)

	sim := newChainSim(usdt.MainnetAddress)
	srv := rpcServerFor(t, sim)

	// Agreeing: the chain holds what we recorded.
	sim.mu.Lock()
	sim.usdt[top.Address] = big.NewInt(100)
	sim.mu.Unlock()

	rep, err := VerifyOnChain(context.Background(), path, srv, 100, usdt.MainnetAddress)
	if err != nil {
		t.Fatalf("VerifyOnChain: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("matching balances reported %v", rep.Findings)
	}

	// Diverging: the chain holds less than we think.
	sim.mu.Lock()
	sim.usdt[top.Address] = big.NewInt(60)
	sim.mu.Unlock()

	rep, err = VerifyOnChain(context.Background(), path, srv, 100, usdt.MainnetAddress)
	if err != nil {
		t.Fatalf("VerifyOnChain: %v", err)
	}
	var found bool
	for _, f := range rep.Findings {
		if f.Kind == "balance" && strings.Contains(f.Detail, "on-chain 60") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drift not reported: %v", rep.Findings)
	}
}
