package api

import (
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"bsc/store"

	"github.com/google/uuid"
)

func healthOf(t *testing.T, f *fixture) (int, healthView) {
	t.Helper()
	w := f.do("GET", "/healthz", nil)
	var view healthView
	f.json(w, w.Code, &view)
	return w.Code, view
}

func TestHealthyServiceReportsOK(t *testing.T) {
	f := newFixture(t)
	code, view := healthOf(t, f)
	if code != http.StatusOK || view.Status != "ok" {
		t.Fatalf("status %d %+v", code, view)
	}
	if view.Version == "" || view.Store != "ok" {
		t.Fatalf("health = %+v, want it to name the build and the store", view)
	}
	if len(view.Reasons) != 0 {
		t.Fatalf("a healthy service listed reasons: %v", view.Reasons)
	}
}

// Past chain.max_lag_blocks withdrawals are already being refused (§21), so the
// service is genuinely degraded rather than merely busy — and saying so is what
// distinguishes "bsc is refusing my payouts" from "bsc is broken".
func TestHealthIsDegradedWhileTooFarBehind(t *testing.T) {
	f := newFixture(t)
	f.sync.behind = f.srv.opts.MaxLagBlocks + 1

	code, view := healthOf(t, f)
	if code != http.StatusServiceUnavailable || view.Status != "degraded" {
		t.Fatalf("status %d %+v", code, view)
	}
	if view.Behind != f.srv.opts.MaxLagBlocks+1 {
		t.Fatalf("blocks_behind = %d", view.Behind)
	}
	if !strings.Contains(strings.Join(view.Reasons, " "), "withdrawals") {
		t.Fatalf("reasons = %v, want the consequence named", view.Reasons)
	}
}

// A master below the gas floor is the service's *normal* self-healing case: the
// top-up sells collected fees back into gas without being asked. Reporting it
// would cry wolf on something that fixes itself every day.
func TestLowMasterAloneIsNotDegraded(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas:  func() *big.Int { return big.NewInt(1) }, // far below the floor
		Floor:      big.NewInt(50_000_000_000_000_000),       // 0.05
		SwapAmount: big.NewInt(10),                           // and it can afford to swap
	}
	// Give the master the tokens a top-up would sell.
	f.creditMaster(t, big.NewInt(100))

	if code, view := healthOf(t, f); code != http.StatusOK || view.Status != "ok" {
		t.Fatalf("status %d %+v — a low master that can refill itself is not a finding", code, view)
	}
}

// Low *and* unable to refill is the combination nothing self-corrects, and the
// one an operator has to be told about.
func TestLowMasterWithNothingToSellIsDegraded(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas:  func() *big.Int { return big.NewInt(1) },
		Floor:      big.NewInt(50_000_000_000_000_000),
		SwapAmount: big.NewInt(10),
	}
	f.creditMaster(t, big.NewInt(9)) // one short of a swap

	code, view := healthOf(t, f)
	if code != http.StatusServiceUnavailable || view.Status != "degraded" {
		t.Fatalf("status %d %+v", code, view)
	}
	if !strings.Contains(strings.Join(view.Reasons, " "), "does not fix itself") {
		t.Fatalf("reasons = %v", view.Reasons)
	}
}

// Swapping off turns every low master into a human's problem, because the thing
// that would have refilled it is not running.
func TestLowMasterIsDegradedWhenSwappingIsOff(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas:  func() *big.Int { return big.NewInt(1) },
		Floor:      big.NewInt(50_000_000_000_000_000),
		SwapAmount: nil, // disabled
	}
	code, view := healthOf(t, f)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d %+v", code, view)
	}
	if !strings.Contains(strings.Join(view.Reasons, " "), "disabled") {
		t.Fatalf("reasons = %v", view.Reasons)
	}
}

// Before the first poll the balance is unknown, which is not the same as zero.
// Treating "not yet observed" as an empty master would make every start-up
// report degraded for a few seconds.
func TestUnobservedGasIsNotAFinding(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas:  func() *big.Int { return nil },
		Floor:      big.NewInt(50_000_000_000_000_000),
		SwapAmount: big.NewInt(10),
	}
	if code, _ := healthOf(t, f); code != http.StatusOK {
		t.Fatalf("status %d, want an unobserved balance to be silent", code)
	}
}

// The zero Health is a service wired without a watcher — the tests' default and
// the inspect path — and must not invent a finding.
func TestHealthWithoutAGasSourceIsQuiet(t *testing.T) {
	f := newFixture(t)
	if code, view := healthOf(t, f); code != http.StatusOK || len(view.Reasons) != 0 {
		t.Fatalf("status %d %+v", code, view)
	}
}

// creditMaster gives the master wallet a token balance, which is what a gas
// top-up would sell. The master is recorded like any other wallet precisely so
// its collected fees are visible this way.
func (f *fixture) creditMaster(t *testing.T, amount *big.Int) {
	t.Helper()
	master, err := f.srv.ring.Master()
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	// Mirrors app.ensureMasterWallet: the master is KindMaster and belongs to no
	// app, which is also why it needs no slug.
	if err := f.st.Update(func(tx *store.Tx) error {
		return tx.PutWallet(store.Wallet{
			ID:        uuid.NewSHA1(uuid.NameSpaceOID, master.Address.Bytes()),
			Kind:      store.KindMaster,
			Address:   master.Address,
			Balance:   amount,
			CreatedAt: time.Now().UTC(),
		})
	}); err != nil {
		t.Fatalf("credit master: %v", err)
	}
}
