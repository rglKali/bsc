package store

import (
	"errors"
	"fmt"

	"bsc/money"
)

var (
	// ErrBelowMinimum means the request is under the app's own minimum, which
	// doubles as the brake on withdrawal spam: a thousand dust payouts is a
	// thousand transactions of the master's gas.
	ErrBelowMinimum = errors.New("store: amount is below the app's minimum")

	// ErrFeeExceedsAmount means deducting the fee would leave nothing to send.
	ErrFeeExceedsAmount = errors.New("store: fee is not smaller than the amount")

	// ErrUnsupportedPolicy means the policy uses a field this version does not
	// implement yet. Rejecting loudly beats quietly charging the wrong fee.
	ErrUnsupportedPolicy = errors.New("store: fee policy field not implemented")
)

// Quote is what a withdrawal will cost, in cents throughout.
type Quote struct {
	Amount money.Cents // what the app asked for
	Fee    money.Cents // the business fee charged on it
	Payout money.Cents // what the destination receives, on-chain
	Debit  money.Cents // what leaves the app's ledger in total
}

// Quote prices a withdrawal under this policy.
//
// The fee is a USDT charge on the app's own users — an exchange-style flat "1
// USDT to withdraw" — and has nothing to do with the BNB the master spends on
// gas, which is identical whether the fee is one dollar or zero.
//
// Direction is the app's choice:
//
//	deduct_fee=false  destination receives `amount`,       ledger debited amount+fee
//	deduct_fee=true   destination receives `amount-fee`,   ledger debited amount
//
// Payout is what moves on-chain; Debit is what the ledger loses. They differ by
// exactly the fee, and that difference is the fee: it is collected by staying in
// the wallet uncredited rather than by a transfer of its own (§24).
func (p FeePolicy) Quote(amount money.Cents, deductFee bool) (Quote, error) {
	if amount <= 0 {
		return Quote{}, fmt.Errorf("store: amount must be positive")
	}
	// v2.1 implements flat and min. The other fields exist so a percentage fee
	// needs no migration later, and are refused until then rather than silently
	// ignored.
	if p.BPS != 0 {
		return Quote{}, fmt.Errorf("%w: bps", ErrUnsupportedPolicy)
	}
	if p.MaxFee != 0 {
		return Quote{}, fmt.Errorf("%w: max_fee_cents", ErrUnsupportedPolicy)
	}
	if amount < p.Min {
		return Quote{}, fmt.Errorf("%w: %d < %d", ErrBelowMinimum, amount, p.Min)
	}

	q := Quote{Amount: amount, Fee: p.Flat}
	if deductFee {
		if p.Flat >= amount {
			return Quote{}, fmt.Errorf("%w: fee %d, amount %d", ErrFeeExceedsAmount, p.Flat, amount)
		}
		q.Payout = amount - p.Flat
		q.Debit = amount
	} else {
		q.Payout = amount
		q.Debit = amount + p.Flat
	}
	return q, nil
}
