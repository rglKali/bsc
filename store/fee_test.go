package store

import (
	"errors"
	"testing"

	"bsc/money"
)

func policy(flat, min money.Cents) FeePolicy {
	return FeePolicy{Flat: flat, Min: min}
}

func TestQuoteChargesOnTop(t *testing.T) {
	q, err := policy(100, 0).Quote(1000, false)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Payout != 1000 {
		t.Fatalf("payout = %d, want the full amount", q.Payout)
	}
	if q.Debit != 1100 {
		t.Fatalf("debit = %d, want amount+fee", q.Debit)
	}
}

func TestQuoteDeductsFromTheAmount(t *testing.T) {
	q, err := policy(100, 0).Quote(1000, true)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Payout != 900 {
		t.Fatalf("payout = %d, want amount-fee", q.Payout)
	}
	if q.Debit != 1000 {
		t.Fatalf("debit = %d, want the amount", q.Debit)
	}
}

// TestPayoutAndDebitAlwaysDifferByTheFee pins the identity the whole fee model
// rests on: what leaves the ledger less what moves on-chain *is* the fee. That
// difference is not transferred anywhere — it stays in the wallet, uncredited,
// and the house sweep collects it later (§24).
func TestPayoutAndDebitAlwaysDifferByTheFee(t *testing.T) {
	for _, deduct := range []bool{false, true} {
		q, err := policy(300, 0).Quote(10_000, deduct)
		if err != nil {
			t.Fatalf("Quote: %v", err)
		}
		if got := q.Debit - q.Payout; got != q.Fee {
			t.Fatalf("deduct=%v: debit-payout = %d, want the fee %d", deduct, got, q.Fee)
		}
	}
}

func TestQuoteWithNoFee(t *testing.T) {
	// An app that sets no policy simply does not charge its users.
	q, err := FeePolicy{}.Quote(1000, false)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Fee != 0 || q.Payout != q.Debit {
		t.Fatalf("free withdrawal quoted as %+v", q)
	}
}

func TestQuoteEnforcesTheMinimum(t *testing.T) {
	if _, err := policy(100, 10_000).Quote(9_999, false); !errors.Is(err, ErrBelowMinimum) {
		t.Fatalf("got %v, want ErrBelowMinimum", err)
	}
	if _, err := policy(100, 10_000).Quote(10_000, false); err != nil {
		t.Fatalf("exactly at the minimum: %v", err)
	}
}

func TestDeductingAFeeMustLeaveSomething(t *testing.T) {
	for _, amount := range []money.Cents{1, 500} {
		if _, err := policy(500, 0).Quote(amount, true); !errors.Is(err, ErrFeeExceedsAmount) {
			t.Fatalf("amount %d: got %v, want ErrFeeExceedsAmount", amount, err)
		}
	}
	if _, err := policy(500, 0).Quote(501, true); err != nil {
		t.Fatalf("amount just above the fee: %v", err)
	}
}

func TestUnimplementedPolicyFieldsAreRefused(t *testing.T) {
	// Better a loud refusal than quietly charging a flat fee where a percentage
	// was configured.
	for name, p := range map[string]FeePolicy{
		"bps":     {Flat: 100, BPS: 100},
		"fee cap": {Flat: 100, MaxFee: 500},
	} {
		if _, err := p.Quote(10_000, false); !errors.Is(err, ErrUnsupportedPolicy) {
			t.Fatalf("%s: got %v, want ErrUnsupportedPolicy", name, err)
		}
	}
}

func TestQuoteRejectsNonPositiveAmounts(t *testing.T) {
	for _, amount := range []money.Cents{0, -1} {
		if _, err := policy(100, 0).Quote(amount, false); err == nil {
			t.Fatalf("amount %d accepted", amount)
		}
	}
}

// TestQuoteIsAPureValue is what replaces the old aliasing test. Cents are
// machine integers, so a quote cannot share mutable state with the policy that
// produced it — the bug class is gone rather than guarded against.
func TestQuoteIsAPureValue(t *testing.T) {
	p := policy(700, 0)
	q, err := p.Quote(1000, false)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	q.Fee = 99_900
	if p.Flat != 700 {
		t.Fatalf("policy mutated through the quote: %d", p.Flat)
	}
}
