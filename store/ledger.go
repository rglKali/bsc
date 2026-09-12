package store

import (
	"fmt"

	"bsc/money"
)

// The ledger: what each app is owed, in cents.
//
// It is the only authority on what an app may spend. The chain can say how much
// a wallet holds but never whose it is — one hot wallet carries the app's money,
// the house's fees and sub-cent dust in a single number — so ownership is
// tracked here and custody is tracked on the wallet (§22).
//
// Every entry is a whole number of cents by construction: deposits are floored
// once on arrival, payouts and fees are quoted in cents. Nothing here can
// produce a fraction, which is what lets Verify recompute the whole balance from
// log/ and compare it against the materialised figure.

// CreditLedger adds a settled deposit's cents to what an app is owed. It is
// called when a drain lands, not when the deposit is seen: money still sitting
// in a deposit address cannot be paid out of the hot wallet, so crediting it
// earlier would authorise a payout with nothing behind it.
func (t *Tx) CreditLedger(slug string, amount money.Cents) (App, error) {
	if amount < 0 {
		return App{}, fmt.Errorf("store: cannot credit a negative amount (%d)", amount)
	}
	return t.MutateApp(slug, func(a *App) error {
		a.Ledger += amount
		return nil
	})
}

// DebitLedger removes a settled withdrawal's cents. The caller passes the figure
// the withdrawal recorded rather than recomputing it, so a fee-policy change
// between request and settlement cannot move it.
func (t *Tx) DebitLedger(slug string, amount money.Cents) (App, error) {
	if amount < 0 {
		return App{}, fmt.Errorf("store: cannot debit a negative amount (%d)", amount)
	}
	return t.MutateApp(slug, func(a *App) error {
		if amount > a.Ledger {
			// Only a bug in our own accounting reaches this: the amount was
			// reserved against this same ledger and reservations cannot exceed
			// it. Refusing keeps the books non-negative rather than papering
			// over it with a clamp.
			return fmt.Errorf("%w: debit %d exceeds ledger %d for %q",
				ErrInsufficient, amount, a.Ledger, slug)
		}
		a.Ledger -= amount
		return nil
	})
}

// ReserveLedger holds cents against an app for a withdrawal about to be created,
// refusing to exceed what is spendable. This is the overdraft guard, and it runs
// before anything is signed rather than after a transfer reverts.
func (t *Tx) ReserveLedger(slug string, amount money.Cents) (App, error) {
	if amount < 0 {
		return App{}, fmt.Errorf("store: cannot reserve a negative amount (%d)", amount)
	}
	return t.MutateApp(slug, func(a *App) error {
		if amount > a.Spendable() {
			return fmt.Errorf("%w: need %d, spendable %d", ErrInsufficient, amount, a.Spendable())
		}
		a.Reserved += amount
		return nil
	})
}

// ReleaseLedger returns a reservation, clamping at zero. Callers release exactly
// what the withdrawal recorded, so this should never clamp.
func (t *Tx) ReleaseLedger(slug string, amount money.Cents) (App, error) {
	return t.MutateApp(slug, func(a *App) error {
		if amount > a.Reserved {
			a.Reserved = 0
			return nil
		}
		a.Reserved -= amount
		return nil
	})
}
