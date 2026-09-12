// Package engine is the transactional glue between the pure rules in flow/ and
// the records in store/: it advances a flow, settles it when it finishes, and
// evaluates the declarative work rules to decide what should exist next.
//
// It is a separate package because two callers need it. The watcher advances
// flows when a transaction reaches finality, and the sender advances them when
// it finds an action unnecessary — a wallet that already holds gas, or whose
// allowance is already set. Both paths must settle identically, so the logic
// lives once, here, rather than in either of them.
//
// Every function takes a store transaction and does all its work inside it.
// That is what makes a block atomic: confirmations, balances, deposits, newly
// started flows and the cursor all commit together or not at all.
package engine

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"bsc/flow"
	"bsc/money"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Config carries the operator settings the rules need.
type Config struct {
	// Scale converts between the ledger's cents and the chain's wei. It has no
	// default: a component that was never told the token's decimals must fail
	// at construction rather than silently value everything at one wei.
	Scale money.Scale

	DrainThreshold *big.Int       // don't spend gas moving less than this
	FeeCollector   common.Address // where the house's money lands

	// HouseSweepMin is how much excess over an app's ledger is worth a transfer.
	// Zero disables sweeping entirely, which is safe: the excess is ours either
	// way and simply keeps accumulating in the wallet.
	HouseSweepMin *big.Int

	// Gas top-up. Disabled unless a router is configured: this is the only work
	// the service starts on its own initiative, and the only one whose outcome
	// is a price rather than a yes/no.
	SwapEnabled  bool
	SwapAmount   *big.Int      // tokens to trade in one go
	GasFloor     *big.Int      // native balance below which a top-up is due
	SwapCooldown time.Duration // minimum gap between attempts
}

// Advance applies a flow transition and, when the flow becomes terminal,
// everything settlement implies. ok is false when the transaction reverted, and
// true both for a confirmation and for an action the sender skipped as
// unnecessary.
func Advance(tx *store.Tx, f store.Flow, ok bool, now time.Time) (store.Flow, error) {
	next, err := flow.Next(f, ok)
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
	if err := settle(tx, f, from, confirmed, ok, now); err != nil {
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
// that failed from a withdrawal that never got as far as paying.
func settle(tx *store.Tx, f store.Flow, from store.FlowState, confirmed common.Hash, ok bool, now time.Time) error {
	switch f.Kind {
	case store.FlowDrain:
		return settleDrain(tx, f, confirmed, ok, now)
	case store.FlowWithdrawal:
		return settleWithdrawal(tx, f, from, confirmed, ok, now)
	case store.FlowPrewarm:
		return nil // activation already recorded above
	case store.FlowGasTopUp:
		// Whatever happened, do not try again until the cooldown expires —
		// including on success, since a swap that did not lift the balance
		// above the floor would otherwise re-fire and keep trading.
		return nil
	case store.FlowHouseSweep:
		// Nothing to record: the money was never owed to anybody, so no ledger
		// entry changes. A failure just backs the wallet off so the rule does
		// not re-fire into the same problem every block.
		if !ok {
			return backOff(tx, f.Wallet, now)
		}
		_, err := tx.ClearBackoff(f.Wallet)
		return err
	}
	return fmt.Errorf("engine: cannot settle unknown flow kind %d", f.Kind)
}

func settleDrain(tx *store.Tx, f store.Flow, confirmed common.Hash, ok bool, now time.Time) error {
	if !ok {
		return backOff(tx, f.Wallet, now)
	}
	// One sweep moves the whole balance, so it credits every deposit waiting on
	// this wallet at once — and the money only becomes spendable now, because
	// only now is it in the wallet payouts are drawn from (§22).
	_, cents, err := tx.CreditDeposits(f.App, f.Wallet, confirmed)
	if err != nil {
		return fmt.Errorf("engine: credit deposits: %w", err)
	}
	if cents > 0 {
		if _, err := tx.CreditLedger(f.App, cents); err != nil {
			return fmt.Errorf("engine: credit ledger: %w", err)
		}
	}
	if _, err := tx.ClearBackoff(f.Wallet); err != nil {
		return fmt.Errorf("engine: clear backoff: %w", err)
	}
	return nil
}

func settleWithdrawal(tx *store.Tx, f store.Flow, from store.FlowState, confirmed common.Hash, ok bool, now time.Time) error {
	// A withdrawal that never reached its payout has not been decided: the
	// funding or the approval failed, and the request stays queued for the
	// rules to retry. Backing the wallet off is what keeps that retry from
	// becoming a loop that burns gas as fast as blocks arrive.
	if from != store.StatePaying {
		if ok {
			return fmt.Errorf("engine: withdrawal flow %s ended in %s without paying", f.ID, from)
		}
		return backOff(tx, f.Wallet, now)
	}

	wd, found, err := tx.Withdrawal(f.Withdrawal)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("engine: withdrawal %s vanished", f.Withdrawal)
	}

	// Release exactly what this record reserved, rather than recomputing it: a
	// policy change between request and settlement must not move the figure.
	if _, err := tx.ReleaseLedger(f.App, wd.Debit); err != nil {
		return fmt.Errorf("engine: release reserve: %w", err)
	}
	if ok {
		// Payout and fee leave the ledger together. Only the payout moved
		// on-chain; the fee stays in the wallet, uncredited, which is precisely
		// how it is collected (§24).
		if _, err := tx.DebitLedger(f.App, wd.Debit); err != nil {
			return fmt.Errorf("engine: debit ledger: %w", err)
		}
		if _, err := tx.ClearBackoff(f.Wallet); err != nil {
			return fmt.Errorf("engine: clear backoff: %w", err)
		}
	}

	_, err = tx.MutateWithdrawal(wd.ID, func(w *store.Withdrawal) error {
		if ok {
			w.Status = store.WithdrawalDone
		} else {
			w.Status = store.WithdrawalFailed
			w.Error = f.Error
		}
		w.TxHash = confirmed
		return nil
	})
	return err
}

// backOff stamps the wallet with an exponentially growing retry deadline. This
// is what stops a declarative work rule plus a failing flow from becoming a
// retry loop that burns gas as fast as blocks arrive.
func backOff(tx *store.Tx, wallet uuid.UUID, now time.Time) error {
	w, found, err := tx.Wallet(wallet)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("engine: wallet %s vanished", wallet)
	}
	delay := flow.RetryDelay(w.FailedAttempts + 1)
	if _, err := tx.BackOff(wallet, now.Add(delay)); err != nil {
		return fmt.Errorf("engine: back off: %w", err)
	}
	return nil
}

// EvaluateWallet starts a drain if this wallet is owed one, reporting whether
// it did. Callers pass the wallets a block actually touched rather than every
// wallet in the database.
func EvaluateWallet(tx *store.Tx, w store.Wallet, cfg Config, now time.Time) (bool, error) {
	if !flow.ShouldDrain(w, cfg.DrainThreshold, now) {
		return false, nil
	}
	app, found, err := tx.App(w.App)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("engine: wallet %s belongs to unknown app %q", w.ID, w.App)
	}
	top, found, err := tx.Wallet(app.Wallet)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("engine: app %q has no top-level wallet", w.App)
	}
	return true, begin(tx, flow.Params{
		Kind: store.FlowDrain, Wallet: w.ID, App: w.App,
		To: top.Address, Active: w.Active, Now: now,
	})
}

// EvaluateApp starts a queued withdrawal if one is owed and the app's top-level
// wallet is free, and otherwise considers sweeping the house's excess.
//
// The two are ordered, not raced: one wallet runs one flow at a time, and the
// app's payout is the more urgent use of it. The excess is ours and is not going
// anywhere, so it waits for a quiet moment.
func EvaluateApp(tx *store.Tx, a store.App, cfg Config, now time.Time) (bool, error) {
	top, found, err := tx.Wallet(a.Wallet)
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("engine: app %q has no top-level wallet", a.Slug)
	}
	if !top.Idle() {
		return false, nil
	}

	queued, err := oldestQueued(tx, a.Slug)
	if err != nil {
		return false, err
	}
	if flow.ShouldPay(a, top, queued != nil) {
		return true, begin(tx, flow.Params{
			Kind: store.FlowWithdrawal, Wallet: top.ID, App: a.Slug,
			To: queued.Destination, Amount: cfg.Scale.Wei(queued.Payout),
			Withdrawal: queued.ID, Active: top.Active, Now: now,
		})
	}

	// What the wallet holds beyond what the app is owed is the house's: the
	// fees withdrawals charged, the sub-cent remainders flooring left behind,
	// and anything a stranger sent to the address. After the ledger they are
	// one quantity and one rule collects them all (§25).
	excess := cfg.Scale.Excess(top.Balance, a.Ledger)
	if flow.ShouldSweepHouse(top, excess, cfg.HouseSweepMin, queued != nil, now) {
		return true, begin(tx, flow.Params{
			Kind: store.FlowHouseSweep, Wallet: top.ID, App: a.Slug,
			To: cfg.FeeCollector, Active: top.Active, Now: now,
		})
	}

	return false, nil
}

// EvaluateGas starts a gas top-up if the master is running low. The native
// balance is passed in because it cannot be derived from logs — an operator's
// manual top-up is a plain value transfer that emits nothing — so the caller
// supplies whatever its last poll saw.
func EvaluateGas(tx *store.Tx, cfg Config, master store.Wallet, native *big.Int, now time.Time) (bool, error) {
	if !flow.ShouldTopUpGas(master, native, cfg.GasFloor, cfg.SwapAmount, cfg.SwapEnabled, now) {
		return false, nil
	}
	// Stamp the cooldown as the flow starts, not when it finishes, so a crash
	// mid-swap cannot produce a burst of attempts on restart.
	if _, err := tx.BackOff(master.ID, now.Add(cfg.SwapCooldown)); err != nil {
		return false, err
	}
	return true, begin(tx, flow.Params{
		Kind: store.FlowGasTopUp, Wallet: master.ID, App: master.App,
		Amount: cfg.SwapAmount, Active: master.Active, Now: now,
	})
}

// EvaluateAll runs every rule over every app and wallet. It is the startup pass
// and the safety net: because the rules read current state rather than react to
// events, one sweep converges whatever was missed while the process was down.
func EvaluateAll(tx *store.Tx, cfg Config, now time.Time) (int, error) {
	started := 0
	apps, err := tx.Apps()
	if err != nil {
		return 0, err
	}
	for _, a := range apps {
		ok, err := EvaluateApp(tx, a, cfg, now)
		if err != nil {
			return started, err
		}
		if ok {
			started++
		}
	}
	var wallets []store.Wallet
	if err := tx.EachWallet(func(w store.Wallet) error {
		wallets = append(wallets, w)
		return nil
	}); err != nil {
		return started, err
	}
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

// oldestQueued returns the app's longest-waiting queued withdrawal, or nil.
func oldestQueued(tx *store.Tx, slug string) (*store.Withdrawal, error) {
	open, err := tx.OpenWithdrawals(slug)
	if err != nil {
		return nil, err
	}
	var queued []store.Withdrawal
	for _, wd := range open {
		if wd.Status == store.WithdrawalQueued {
			queued = append(queued, wd)
		}
	}
	if len(queued) == 0 {
		return nil, nil
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].CreatedAt.Before(queued[j].CreatedAt) })
	return &queued[0], nil
}

// begin creates a flow and claims its wallet, the two halves of "this wallet is
// now busy" that must never be separated.
func begin(tx *store.Tx, p flow.Params) error {
	f, err := flow.Begin(p)
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
