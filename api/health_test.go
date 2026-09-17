package api

import (
	"math/big"
	"net/http"
	"strings"
	"testing"
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

// A master below the gas floor used to be self-healing: the automatic top-up
// sold collected fees back into gas, so reporting it would have cried wolf.
// Nothing refills it automatically any more, so being low *is* the finding —
// it is the signal to go and run `bsc swap` (§38).
func TestLowMasterIsDegraded(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas: func() *big.Int { return big.NewInt(1) }, // far below the floor
		Floor:     big.NewInt(50_000_000_000_000_000),       // 0.05
	}

	code, view := healthOf(t, f)
	if code != http.StatusServiceUnavailable || view.Status != "degraded" {
		t.Fatalf("status %d %+v", code, view)
	}
	if !strings.Contains(strings.Join(view.Reasons, " "), "gas floor") {
		t.Fatalf("reasons = %v", view.Reasons)
	}
}

func TestMasterAboveTheFloorIsQuiet(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas: func() *big.Int { return big.NewInt(60_000_000_000_000_000) },
		Floor:     big.NewInt(50_000_000_000_000_000),
	}
	if code, view := healthOf(t, f); code != http.StatusOK || view.Status != "ok" {
		t.Fatalf("status %d %+v", code, view)
	}
}

// Before the first poll the balance is unknown, which is not the same as zero.
// Treating "not yet observed" as an empty master would make every start-up
// report degraded for a few seconds.
func TestUnobservedGasIsNotAFinding(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.Health = Health{
		MasterGas: func() *big.Int { return nil },
		Floor:     big.NewInt(50_000_000_000_000_000),
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
