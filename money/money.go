// Package money holds bsc's one unit and the parsing that guards it.
//
// There is only one unit now: the token's own integer base units, the same
// number every `Transfer` log, every `balanceOf` and every `transferFrom`
// carries. It is what the chain says, bit for bit, and bsc neither scales it
// nor rounds it.
//
// This replaces the cents ledger and the wei/cents conversion beside it. That
// pair existed to answer "what is this app allowed to spend", which is an
// accounting question — and accounting is not this service's job. A caller that
// needs dollars, fees or per-user balances keeps its own books; bsc reports
// what arrived and moves what it is told to move. Flooring to cents was the
// only rounding in the service and the only place value could go missing, and
// with the ledger gone there is nothing left to floor.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// ErrBadAmount means a wire amount was not a plain non-negative integer.
var ErrBadAmount = errors.New("money: amount must be a whole number of base units")

// maxDigits bounds what Parse will look at. A uint256 is 78 decimal digits, so
// anything longer cannot be a token amount and is rejected before big.Int is
// asked to allocate for it.
const maxDigits = 78

// Parse reads a wire amount: a plain non-negative integer of base units, as a
// decimal string.
//
// Strings, not JSON numbers, because a uint256 does not survive a float64 —
// and decimal-pointed input is refused rather than scaled, since bsc does not
// know how many decimals the caller had in mind and guessing would move money.
func Parse(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: empty", ErrBadAmount)
	}
	if len(s) > maxDigits {
		return nil, fmt.Errorf("%w: %d digits exceeds a uint256", ErrBadAmount, len(s))
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil, fmt.Errorf("%w: %q", ErrBadAmount, s)
		}
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadAmount, s)
	}
	return v, nil
}

// ParsePositive is Parse plus a non-zero requirement, for amounts that must
// actually move something.
func ParsePositive(s string) (*big.Int, error) {
	v, err := Parse(s)
	if err != nil {
		return nil, err
	}
	if v.Sign() == 0 {
		return nil, fmt.Errorf("%w: must be greater than zero", ErrBadAmount)
	}
	return v, nil
}

// String renders an amount for the wire. A nil amount is "0" so callers never
// have to nil-check a balance they are about to print.
func String(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// OrZero makes a nil amount usable without nil-checking arithmetic everywhere.
func OrZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
