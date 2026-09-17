package api

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Amounts on the wire.
//
// There is one unit: the token's own integer base units, the same number every
// `Transfer` log, every `balanceOf` and every `transferFrom` carries. bsc
// neither scales it nor rounds it, so this file is only ever parsing and
// printing — there is no conversion anywhere, and that is the point (§36).
//
// This lives in api/ because it is a wire concern. The store keeps *big.Int and
// the chain keeps uint256; the decimal string exists solely because a uint256
// does not survive JSON's float64.

// errBadAmount means a wire amount was not a plain non-negative integer.
var errBadAmount = errors.New("amount must be a whole number of base units")

// maxAmountDigits bounds what parseAmount will look at. A uint256 is 78 decimal
// digits, so anything longer cannot be a token amount and is rejected before
// big.Int is asked to allocate for it.
const maxAmountDigits = 78

// parseAmount reads a wire amount: a plain non-negative integer of base units,
// as a decimal string.
//
// Strings, not JSON numbers, because a uint256 does not survive a float64 — and
// decimal-pointed input is refused rather than scaled, since bsc does not know
// how many decimals the caller had in mind and guessing would move money.
func parseAmount(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("%w: empty", errBadAmount)
	}
	if len(s) > maxAmountDigits {
		return nil, fmt.Errorf("%w: %d digits exceeds a uint256", errBadAmount, len(s))
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil, fmt.Errorf("%w: %q", errBadAmount, s)
		}
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", errBadAmount, s)
	}
	return v, nil
}

// parsePositiveAmount is parseAmount plus a non-zero requirement, for amounts
// that must actually move something.
func parsePositiveAmount(s string) (*big.Int, error) {
	v, err := parseAmount(s)
	if err != nil {
		return nil, err
	}
	if v.Sign() == 0 {
		return nil, fmt.Errorf("%w: must be greater than zero", errBadAmount)
	}
	return v, nil
}

// amountString renders an amount for the wire. A nil amount is "0" so callers
// never have to nil-check a balance they are about to print.
func amountString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// orZero makes a nil amount usable without nil-checking arithmetic everywhere.
func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
