package api

import (
	"math/big"
	"net/http"

	"bsc/buildinfo"
	"bsc/store"
)

// Health is what /healthz needs beyond the store, supplied by the watcher.
//
// Only the master's *native* balance has to be handed in. Everything else the
// check wants is either in the store or already on the server: token balances
// are maintained from logs, and the lag is what Sync reports. The native
// balance is the one quantity in the service that cannot be derived, because a
// manual gas top-up is a plain value transfer and emits nothing (§14).
type Health struct {
	// MasterGas returns the last observed native balance, or nil if it has not
	// been observed yet — which is not the same as zero and must not be read as
	// an empty master.
	MasterGas func() *big.Int

	// Floor is swap.gas_floor_wei: below this, a top-up is due.
	Floor *big.Int

	// SwapAmount is swap.amount_wei — the tokens a top-up has to sell. Nil when
	// swapping is disabled, which is what turns a low master from a condition
	// that fixes itself into one that does not.
	SwapAmount *big.Int
}

// healthView is deliberately more than "ok". A health check that cannot fail
// only tells you the process is running, which is the one thing a connection
// refusal already told you.
type healthView struct {
	Status  string   `json:"status"` // ok | degraded
	Version string   `json:"version"`
	Store   string   `json:"store"`
	Behind  uint64   `json:"blocks_behind"`
	Reasons []string `json:"degraded,omitempty"`
}

// getHealth reports whether the service can still do its job.
//
// Three things make it useless while it is still answering requests, and each
// is reported rather than merely counted somewhere:
//
//   - the store will not answer, so nothing can be read or written;
//   - the watcher is far enough behind that withdrawals are being refused,
//     because balances are no longer current (§21);
//   - the master cannot pay for gas and cannot fix that itself.
//
// That last one is the subtle one and is why this check exists at all. A low
// master is normally self-healing: the top-up sells collected fees back into
// gas. It is only a human's problem when it is low *and* there is nothing to
// sell — or swapping is off — which is exactly the combination the Grafana
// panel guidance calls out. Reporting a low master on its own would cry wolf on
// a condition the service fixes by itself every day.
//
// **This is for monitoring, not for restarting.** A 503 here means "do not send
// this traffic and look at me", never "bounce me": nothing it reports is fixed
// by starting the process again, and a restart mid-sync makes the lag worse.
func (s *Server) getHealth(w http.ResponseWriter, _ *http.Request) error {
	view := healthView{Status: "ok", Version: buildinfo.Version, Store: "ok"}

	// Cheapest possible proof the store is readable and consistent, and the one
	// failure that leaves the process up but unable to do anything.
	var apps []store.App
	if err := s.store.View(func(tx *store.Tx) error {
		var err error
		apps, err = tx.Apps()
		return err
	}); err != nil {
		view.Status, view.Store = "degraded", err.Error()
		view.Reasons = append(view.Reasons, "the database will not answer")
		// No point evaluating anything else: every other check reads it.
		writeJSON(w, http.StatusServiceUnavailable, view)
		return nil
	}
	_ = apps

	view.Behind = s.sync.Behind()
	if view.Behind > s.opts.MaxLagBlocks {
		view.Status = "degraded"
		view.Reasons = append(view.Reasons,
			"chain sync is behind; withdrawals are being refused until it catches up")
	}

	if reason, ok := s.gasTrouble(); ok {
		view.Status = "degraded"
		view.Reasons = append(view.Reasons, reason)
	}

	code := http.StatusOK
	if view.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, view)
	return nil
}

// gasTrouble reports a master that is low on gas *and* cannot recover on its
// own. A master that is merely low is not a finding: the top-up exists for
// exactly that and runs without being asked.
func (s *Server) gasTrouble() (string, bool) {
	h := s.opts.Health
	if h.MasterGas == nil || h.Floor == nil || h.Floor.Sign() <= 0 {
		return "", false // nothing to judge against
	}
	gas := h.MasterGas()
	if gas == nil {
		return "", false // not observed yet; absence of news is not bad news
	}
	if gas.Cmp(h.Floor) >= 0 {
		return "", false
	}

	// Below the floor. Whether that is a problem depends entirely on whether a
	// top-up can still happen.
	if h.SwapAmount == nil || h.SwapAmount.Sign() <= 0 {
		return "the master is below the gas floor and gas top-ups are disabled: " +
			"nothing will refill it", true
	}

	// A top-up sells the master's own collected fees, so the question is
	// whether it holds enough of them.
	var held *big.Int
	if err := s.store.View(func(tx *store.Tx) error {
		master, err := s.ring.Master()
		if err != nil {
			return err
		}
		wl, found, err := tx.WalletByAddress(master.Address)
		if err != nil || !found {
			return err
		}
		held = wl.Balance
		return nil
	}); err != nil {
		return "", false // a store failure is already reported above
	}
	if held == nil {
		held = new(big.Int)
	}
	if held.Cmp(h.SwapAmount) < 0 {
		return "the master is below the gas floor and holds too little to swap for more: " +
			"this is the combination that does not fix itself", true
	}
	return "", false
}
