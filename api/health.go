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

	// Floor is gas.floor_wei: below this, the master needs refilling.
	Floor *big.Int
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
//   - the master is below the gas floor, so it can no longer pay to move
//     anything.
//
// That last check used to be a conjunction — low *and* unable to recover —
// because the automatic top-up meant a low master normally fixed itself, and
// reporting it would have cried wolf. With trading moved to an operator command
// nothing fixes it automatically any more, so being low is exactly the finding:
// it is the signal to go and run `bsc swap` (§38).
//
// **This is for monitoring, not for restarting.** A 503 here means "do not send
// this traffic and look at me", never "bounce me": nothing it reports is fixed
// by starting the process again, and a restart mid-sync makes the lag worse.
func (s *Server) getHealth(w http.ResponseWriter, _ *http.Request) error {
	view := healthView{Status: "ok", Version: buildinfo.Version, Store: "ok"}

	// Cheapest possible proof the store is readable and consistent, and the one
	// failure that leaves the process up but unable to do anything.
	if err := s.store.View(func(tx *store.Tx) error {
		_, err := tx.Wallets("", 1)
		return err
	}); err != nil {
		view.Status, view.Store = "degraded", err.Error()
		view.Reasons = append(view.Reasons, "the database will not answer")
		// No point evaluating anything else: every other check reads it.
		writeJSON(w, http.StatusServiceUnavailable, view)
		return nil
	}

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

// gasTrouble reports a master below the gas floor. Nothing refills it without
// being asked, so low is the whole finding.
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

	return "the master is below the gas floor: it cannot pay for transfers until it is refilled", true
}
