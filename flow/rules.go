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
// The payoff is convergence. Two deposits in one block are applied before the
// check runs, so the wallet is evaluated once with the summed balance and
// exactly one drain starts. A deposit landing mid-drain finds the wallet busy
// and starts nothing — and the evaluation after that drain terminates picks up
// the leftover, so funds cannot be stranded by arriving between the balance
// read and the signature.
//
// Two rules are gone with the ledger. The house sweep computed `custody −
// ledger`, which bsc can no longer evaluate; and the gas top-up traded tokens
// for gas on its own initiative, which is now an operator command instead
// (§33, §38).

// Backoff schedule for a failed flow. Without it, a declarative rule plus a
// flow that can fail is a retry loop that burns gas as fast as the chain
// allows.
const (
	backoffBase = 30 * time.Second
	backoffMax  = 6 * time.Hour
)

// RetryDelay returns how long to wait before retrying a wallet's flow, doubling
// per failure up to a cap. The cap matters as much as the growth: a wallet that
// can never drain (a broken token, a blacklisted address) must keep retrying
// occasionally rather than either hammering or giving up forever.
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
// The DrainTo check is what used to be a wallet-kind check. It is load-bearing
// either way: a wallet with no destination has nowhere to drain to, and an
// unscoped rule would try to move its balance to itself forever. Making it a
// field rather than a kind is what lets any wallet be either thing (§32).
//
// The threshold is pure gas economics — moving three cents costs more than
// three cents. It decides only whether moving the money is worth paying for;
// the deposit is recorded either way.
func ShouldDrain(w store.Wallet, threshold *big.Int, now time.Time) bool {
	switch {
	case w.Kind != store.KindManaged:
		return false
	case !w.Proxies():
		return false
	case !w.Idle():
		return false
	case now.Before(w.RetryAfter):
		return false
	}
	return cmp(w.Balance, threshold) >= 0 && sign(w.Balance) > 0
}

// ShouldPay reports whether a wallet should start a pending withdrawal.
// Withdrawals from one wallet therefore serialize, which is also what keeps the
// overdraft arithmetic obvious.
//
// A proxy wallet is refused outright: its balance is on its way somewhere else,
// and paying out of it would race the drain for the same funds. That is the
// mutual exclusion between forwarding and accumulating, enforced where it
// matters rather than only documented (§32).
//
// Pausing blocks payouts. Drains carry on regardless — those are the topology
// doing what it was configured to do, not a caller spending.
//
// The RetryAfter gate is load-bearing rather than symmetry with ShouldDrain. A
// withdrawal has no failure state: a reverted payout stays `pending` for this
// rule to pick up again (§28), so without the gate a payout that reverts every
// time would be re-signed on every evaluation and burn the master's gas as fast
// as blocks arrive.
func ShouldPay(w store.Wallet, hasPending bool, now time.Time) bool {
	switch {
	case !hasPending:
		return false
	case w.Kind != store.KindManaged:
		return false
	case w.Proxies():
		return false
	case w.Paused:
		return false
	case !w.Idle():
		return false
	case now.Before(w.RetryAfter):
		return false
	}
	return true
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
