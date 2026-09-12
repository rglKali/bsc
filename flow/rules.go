package flow

import (
	"math/big"
	"time"

	"bsc/store"
)

// Work is declared, not enqueued. Rather than "a deposit creates a drain job",
// these predicates say what *should* exist given the current state, and the
// coordinator evaluates them at three points: inside the block transaction,
// immediately after any flow terminates, and once at startup.
//
// Fee collection is deliberately *not* one of these. It is a state of the
// withdrawal that earned the fee, taken while that flow still holds the wallet,
// so it can never queue behind the next payout — which is exactly what a
// separate rule would have let happen.
//
// The payoff is convergence. Two deposits in one block are credited before the
// check runs, so the wallet is evaluated once with the summed balance and
// exactly one drain starts. A deposit landing mid-drain finds the wallet busy
// and starts nothing — and the evaluation after that drain terminates picks up
// the leftover, so funds cannot be stranded by arriving between the balance
// read and the signature.

// Backoff schedule for a failed drain. Without it, a declarative rule plus a
// flow that can fail is a retry loop that burns gas as fast as the chain
// allows.
const (
	backoffBase = 30 * time.Second
	backoffMax  = 6 * time.Hour
)

// RetryDelay returns how long to wait before retrying a wallet's flow, doubling
// per failure up to a cap. The cap matters as much as the growth: a
// wallet that can never drain (a broken token, a blacklisted address) must keep
// retrying occasionally rather than either hammering or giving up forever.
func RetryDelay(attempts uint32) time.Duration {
	if attempts == 0 {
		return 0
	}
	d := backoffBase
	for range attempts - 1 {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	return d
}

// ShouldDrain reports whether a wallet is owed a drain flow.
//
// The kind check is load-bearing rather than cosmetic: an app's top-level
// wallet also carries a balance, and an unscoped rule would try to drain it to
// itself forever.
//
// The threshold is pure gas economics — moving three cents costs more than three
// cents — and has nothing to do with what the app was credited. A deposit is
// credited the moment it is worth a whole cent; when we physically move it is
// our own business, and until we do it shows as pending (§22).
func ShouldDrain(w store.Wallet, threshold *big.Int, now time.Time) bool {
	switch {
	case w.Kind != store.KindDeposit:
		return false
	case !w.Idle():
		return false
	case now.Before(w.RetryAfter):
		return false
	}
	return cmp(w.Balance, threshold) >= 0 && sign(w.Balance) > 0
}

// ShouldSweepHouse reports whether a wallet holds enough beyond its app's ledger
// to be worth collecting.
//
// The excess is everything on-chain that no app is owed: the fees withdrawals
// charged, the sub-cent remainders flooring left behind, and anything a stranger
// sent to a managed address. One rule collects all of it, because after the
// ledger they are the same thing (§25).
//
// It defers to a queued withdrawal. The app's own payout is the more urgent use
// of a wallet that can only run one flow at a time, and the excess is not going
// anywhere.
func ShouldSweepHouse(top store.Wallet, excess, threshold *big.Int, hasQueued bool, now time.Time) bool {
	switch {
	case top.Kind != store.KindTopLevel:
		return false
	case !top.Idle():
		return false
	case hasQueued:
		return false
	case now.Before(top.RetryAfter):
		return false
	case sign(threshold) <= 0:
		return false // sweeping is off
	}
	return cmp(excess, threshold) >= 0
}

// ShouldPay reports whether an app's top-level wallet should start a queued
// withdrawal. Withdrawals for one app therefore serialize, which is also what
// keeps the reserve arithmetic obvious.
//
// Pausing an app blocks payouts. Drains carry on regardless — those are us
// collecting our own money, not the app spending its users'.
func ShouldPay(app store.App, top store.Wallet, hasQueued bool) bool {
	return hasQueued && !app.Paused && top.Idle() && top.Kind == store.KindTopLevel
}

func sign(v *big.Int) int {
	if v == nil {
		return 0
	}
	return v.Sign()
}

func cmp(a, b *big.Int) int {
	if a == nil {
		a = new(big.Int)
	}
	if b == nil {
		b = new(big.Int)
	}
	return a.Cmp(b)
}

// ShouldTopUpGas reports whether the master should trade collected fees for gas.
//
// This is the only work the service starts purely on its own initiative —
// everything else traces back to a deposit arriving or an app asking. It is
// therefore hedged on every side: it needs a configured router, a balance below
// the floor, enough tokens to trade, an idle master, and a cooldown since the
// last attempt.
//
// The cooldown matters more than it looks. A swap that succeeds but does not
// lift the balance above the floor — because the floor is set too high, or the
// trade was too small — would otherwise re-fire immediately and keep trading
// away fees until there were none left. Reusing the wallet's retry deadline
// bounds that to one attempt per interval whatever goes wrong.
func ShouldTopUpGas(master store.Wallet, native, floor, swapAmount *big.Int, enabled bool, now time.Time) bool {
	switch {
	case !enabled:
		return false
	case master.Kind != store.KindMaster:
		return false
	case !master.Idle():
		return false
	case now.Before(master.RetryAfter):
		return false
	case sign(swapAmount) <= 0 || sign(floor) <= 0:
		return false
	case cmp(native, floor) >= 0:
		return false // there is enough gas
	}
	// Only trade what we actually hold. The master's token balance is its
	// collected fees, and trading more than that would simply revert.
	return cmp(master.Balance, swapAmount) >= 0
}
