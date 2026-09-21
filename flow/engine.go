// The transactional half of this package: everything here takes a store
// transaction and does all its work inside it, which is the opposite of the
// rules beside it and the reason the two are worth telling apart by file rather
// than by import path.
//
// Two callers need it, which is why it is here rather than in either of them.
// The watcher advances flows when a transaction reaches finality, and the
// sender advances them when it finds an action unnecessary — a wallet that
// already holds gas, or whose allowance is already set. Both paths must settle
// identically.
//
// Every function takes a store transaction and does all its work inside it.
// That is what makes a block atomic: confirmations, balances, deposits, newly
// started flows and the cursor all commit together or not at all.
package flow

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Config carries the operator settings the rules need.
//
// It used to carry the fee collector, the house-sweep minimum, the token scale
// and five swap settings. All of those existed to serve the ledger or the gas
// top-up, and both are gone: what is left is the one number that decides
// whether moving money is worth the gas.
type Config struct {
	DrainThreshold *big.Int // don't spend gas moving less than this
}

// Advance applies a flow transition and, when the flow becomes terminal,
// everything settlement implies. ok is false when the transaction reverted, and
// true both for a confirmation and for an action the sender skipped as
// unnecessary.
func Advance(tx *store.Tx, f store.Flow, block uint64, ok bool, now time.Time) (store.Flow, error) {
	next, err := Next(f, ok)
	if err != nil {
		return f, err
	}

	// Leaving `approving` successfully is what makes a wallet active, whichever
	// kind of flow happened to be the one that activated it.
	if ok && f.State == store.StateApproving {
		if _, err := tx.SetActive(f.Wallet, true); err != nil {
			return f, fmt.Errorf("engine: mark active: %w", err)
		}
	}

	confirmed, from := f.Tx, f.State

	f.State = next
	f.UpdatedAt = now.UTC()

	if !next.IsTerminal() {
		f.Tx = common.Hash{} // the next state owes a new transaction
		return f, tx.PutFlow(f)
	}
	if err := settle(tx, f, from, confirmed, block, ok, now); err != nil {
		return f, err
	}
	// Deleting releases the wallet, which is what makes it eligible for the
	// next round of the work rules — including a deposit that landed mid-drain.
	return f, tx.DeleteFlow(f)
}

// settle applies the consequences of a flow reaching a terminal state. Anything
// worth keeping must be copied onto a durable record here, because the flow
// itself is about to be deleted. `from` is the state the flow was in when the
// transaction it was waiting on resolved, which is what distinguishes a payout
// that reverted from a withdrawal that never got as far as paying. Neither is
// terminal for the request — both retry (§28) — but only the first has a payout
// transaction worth recording.
func settle(tx *store.Tx, f store.Flow, from store.FlowState, confirmed common.Hash, block uint64, ok bool, now time.Time) error {
	switch {
	case f.Kind == store.FlowPrewarm:
		return nil // activation already recorded above
	case f.Kind != store.FlowTransfer:
		return fmt.Errorf("engine: cannot settle unknown flow kind %d", f.Kind)
	// Which of the two a transfer is comes from whether it names a withdrawal,
	// which Begin guarantees travels with the amount (§46).
	case f.Pays():
		return settleWithdrawal(tx, f, from, confirmed, block, ok, now)
	default:
		return settleDrain(tx, f, confirmed, block, ok, now)
	}
}

// settleDrain records the debit the drain performed and links the credits it
// carried to it.
//
// The record is created here rather than when the drain started, and is born
// terminal. A drain is not a promise: nothing was owed to anybody while it was
// in flight, nobody could refuse it, and it never counted against the wallet.
// What there is, once it lands, is an observation — money left this address —
// which is the same kind of fact as a deposit and is recorded the same way
// (§42).
//
// Nothing is credited. Under the ledger this was the moment an app's money
// became spendable; now it is simply the moment the money left one address for
// another.
func settleDrain(tx *store.Tx, f store.Flow, confirmed common.Hash, block uint64, ok bool, now time.Time) error {
	if !ok {
		return backOff(tx, f.Wallet, now)
	}
	// A drain that needed no transaction — an empty wallet the sender skipped —
	// moved nothing, so there is nothing to observe and no debit to record.
	if confirmed == (common.Hash{}) {
		_, err := tx.ClearBackoff(f.Wallet)
		return err
	}
	debit := store.Withdrawal{
		ID: uuid.New(), Wallet: f.Wallet, Reason: store.ReasonDrain,
		Destination: f.To,
		// Resolved when the sweep was signed, from the balance the chain
		// reported then — a drain never carries an amount before that.
		Amount: f.Amount,
		TxHash: confirmed, Block: block,
		CreatedAt: now.UTC(), SettledAt: now.UTC(),
	}
	if err := tx.PutWithdrawal(debit); err != nil {
		return fmt.Errorf("engine: record drain: %w", err)
	}
	// Nothing is stamped on the credits. A drain moves a *balance*, not a set
	// of deposits — the amount is read from balanceOf at signing — so linking
	// individual credits to it was always an approximation, and a wrong one for
	// any deposit that arrived while the sweep was in flight (§50).
	if _, err := tx.ClearBackoff(f.Wallet); err != nil {
		return fmt.Errorf("engine: clear backoff: %w", err)
	}
	return nil
}

func settleWithdrawal(tx *store.Tx, f store.Flow, from store.FlowState, confirmed common.Hash, block uint64, ok bool, now time.Time) error {
	// A withdrawal that never reached the transfer has not been decided: the
	// funding or the approval failed, and the request stays `pending` for the
	// rules to retry. Backing the wallet off is what keeps that retry from
	// becoming a loop that burns gas as fast as blocks arrive.
	if from != store.StateMoving {
		if ok {
			return fmt.Errorf("engine: withdrawal flow %s ended in %s without paying", f.ID, from)
		}
		return backOff(tx, f.Wallet, now)
	}

	wd, found, err := tx.Pending(f.Withdrawal)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("engine: withdrawal %s vanished", f.Withdrawal)
	}

	// A payout that reverted is not the caller's problem to compensate for: the
	// record stays `pending` and the wallet backs off so ShouldPay picks it up
	// again later (§28). Marking it terminal would make it look settled when
	// nothing moved — and while it is pending it still counts against the
	// wallet's committed total, so the overdraft guard keeps holding.
	if !ok {
		_, err := tx.MutatePending(wd.ID, func(p *store.Pending) error {
			// Kept for the operator, not as a verdict: the promise is still
			// live and the next attempt overwrites this.
			p.Error = f.Error
			p.Attempts++
			return nil
		})
		if err != nil {
			return err
		}
		return backOff(tx, f.Wallet, now)
	}

	if _, err := tx.ClearBackoff(f.Wallet); err != nil {
		return fmt.Errorf("engine: clear backoff: %w", err)
	}
	// The wallet's balance is not adjusted here. The watcher observes the
	// outgoing Transfer in the same block and debits custody from the log,
	// which keeps one source of truth for what a wallet holds.
	//
	// Settling MOVES the record: it leaves state/ and arrives in log/ as a
	// fact, in this same transaction. The diagnostics go with the promise —
	// how many attempts it took is a story about keeping it, not about the
	// movement (§51).
	_, err = tx.Settle(wd.ID, confirmed, block, now)
	return err
}

// backOff stamps the wallet with an exponentially growing retry deadline. This
// is what stops a declarative work rule plus a failing flow from becoming a
// retry loop that burns gas as fast as blocks arrive.
func backOff(tx *store.Tx, wallet store.WalletID, now time.Time) error {
	w, found, err := tx.Wallet(wallet)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("engine: wallet %s vanished", wallet)
	}
	delay := RetryDelay(w.FailedAttempts + 1)
	if _, err := tx.BackOff(wallet, now.Add(delay)); err != nil {
		return fmt.Errorf("engine: back off: %w", err)
	}
	return nil
}

// EvaluateWallet starts whatever this wallet is owed, reporting whether it did.
//
// One function covers both rules now, because with the topology on the wallet
// the two cases are mutually exclusive by construction: a wallet either
// forwards what it receives or holds it and pays out. There is no ordering to
// get right between them and no app-level pass above it (§32).
func EvaluateWallet(tx *store.Tx, w store.Wallet, cfg Config, now time.Time) (bool, error) {
	if !w.Idle() {
		return false, nil
	}
	if w.Proxies() {
		if !ShouldDrain(w, cfg.DrainThreshold, now) {
			return false, nil
		}
		return true, startFlow(tx, Params{
			Kind: store.FlowTransfer, Wallet: w.ID,
			To: w.DrainTo, Active: w.Active, Now: now,
		})
	}

	pending, err := oldestPending(tx, w.ID)
	if err != nil {
		return false, err
	}
	if !ShouldPay(w, pending != nil, now) {
		return false, nil
	}
	return true, startFlow(tx, Params{
		Kind: store.FlowTransfer, Wallet: w.ID,
		To: pending.Destination, Amount: pending.Amount,
		Withdrawal: pending.ID, Active: w.Active, Now: now,
	})
}

// EvaluateAll runs every rule over every wallet. It is the startup pass and the
// safety net: because the rules read current state rather than react to events,
// one sweep converges whatever was missed while the process was down.
func EvaluateAll(tx *store.Tx, cfg Config, now time.Time) (int, error) {
	var wallets []store.Wallet
	if err := tx.EachWallet(func(w store.Wallet) error {
		wallets = append(wallets, w)
		return nil
	}); err != nil {
		return 0, err
	}
	started := 0
	for _, w := range wallets {
		ok, err := EvaluateWallet(tx, w, cfg, now)
		if err != nil {
			return started, err
		}
		if ok {
			started++
		}
	}
	return started, nil
}

// oldestPending returns a wallet's longest-waiting pending withdrawal, or nil.
//
// Oldest-first is what stops a withdrawal that keeps reverting from starving
// the ones behind it forever: it is retried rather than failed (§28), so
// without an ordering it could hold the wallet on every evaluation. Its wallet
// backoff lets the queue behind it move in the meantime.
func oldestPending(tx *store.Tx, wallet store.WalletID) (*store.Pending, error) {
	// Everything in this keyspace is pending, so there is nothing to filter:
	// the bucket is the outstanding set (§51).
	open, err := tx.WalletPending(wallet)
	if err != nil || len(open) == 0 {
		return nil, err
	}
	sort.Slice(open, func(i, j int) bool { return open[i].CreatedAt.Before(open[j].CreatedAt) })
	return &open[0], nil
}

// startFlow creates a flow and claims its wallet, the two halves of "this wallet is
// now busy" that must never be separated.
func startFlow(tx *store.Tx, p Params) error {
	f, err := Begin(p)
	if err != nil {
		return err
	}
	if err := tx.PutFlow(f); err != nil {
		return err
	}
	if _, err := tx.ClaimWallet(f.Wallet, f.ID); err != nil {
		return fmt.Errorf("engine: claim wallet: %w", err)
	}
	return nil
}
