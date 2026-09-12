// Package money holds bsc's two units and the single conversion between them.
//
// The service speaks two languages and must never confuse them:
//
//   - **cents** — what an app is owed. Exact, integral, and the only unit that
//     crosses the API. Every ledger entry is a whole number of cents by
//     construction, so the ledger can be recomputed from the log with no
//     rounding anywhere in the arithmetic.
//   - **wei** — what the chain holds. Every log, every balanceOf and every
//     transfer is denominated in it, so anything that touches the chain stays
//     in wei and is compared to the chain bit for bit.
//
// The gap between them is deliberate and is the house's: a transfer of
// 10.007 USDT credits an app 1000 cents and leaves 0.007 behind. Flooring is
// the only rounding in the system, it always rounds towards the house, and it
// happens exactly once — here.
//
// The scale is derived from the token's own decimals() rather than configured.
// A cent is 10^16 wei for BSC's 18-decimal USDT and 10^4 for a 6-decimal one,
// and a constant that can be wrong is a constant that eventually is.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Cents is an amount an app can hold, owe or be paid. int64 reaches ninety
// trillion tokens, far past anything a gateway will custody, and staying a
// machine integer keeps ledger arithmetic exact and cheap.
type Cents int64

// String renders the plain integer the wire uses. Amounts are never floats and
// never decimal-pointed: "150" is a dollar fifty, unambiguously.
func (c Cents) String() string { return strconv.FormatInt(int64(c), 10) }

// Tokens renders a human-readable amount for logs and errors only. Never parse
// it back — the wire format is String.
func (c Cents) Tokens() string {
	neg := ""
	v := int64(c)
	if v < 0 {
		neg, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", neg, v/100, v%100)
}

// ErrBadAmount means a wire amount was not a plain non-negative integer.
var ErrBadAmount = errors.New("money: amount must be a whole number of cents")

// Parse reads a wire amount: a plain non-negative integer of cents. It rejects
// anything decimal-pointed rather than rounding it, because a caller that sent
// "1.5" meant something we would have to guess at.
func Parse(s string) (Cents, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrBadAmount)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrBadAmount, s)
	}
	if v < 0 {
		return 0, fmt.Errorf("%w: %q is negative", ErrBadAmount, s)
	}
	return Cents(v), nil
}

// Scale converts between cents and the wei of one particular token.
type Scale struct {
	cent     *big.Int // wei in one cent
	decimals uint8
}

// ErrDecimals means the token cannot express cents at all.
var ErrDecimals = errors.New("money: token has fewer than 2 decimals")

// NewScale builds the conversion for a token with the given decimals. Two is
// the floor: a token with fewer cannot represent a cent, and every amount in
// this service is a number of cents.
func NewScale(decimals uint8) (Scale, error) {
	if decimals < 2 {
		return Scale{}, fmt.Errorf("%w: %d", ErrDecimals, decimals)
	}
	if decimals > 36 {
		return Scale{}, fmt.Errorf("money: implausible token decimals %d", decimals)
	}
	cent := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)-2), nil)
	return Scale{cent: cent, decimals: decimals}, nil
}

// Valid reports whether this scale was built by NewScale. The zero Scale is
// unusable on purpose: a component that was never told the token's decimals
// must fail at construction, not convert everything by a factor of one.
func (s Scale) Valid() bool { return s.cent != nil && s.cent.Sign() > 0 }

// Decimals is the token's own decimals().
func (s Scale) Decimals() uint8 { return s.decimals }

// CentWei is how many wei make a cent.
func (s Scale) CentWei() *big.Int {
	if !s.Valid() {
		return new(big.Int)
	}
	return new(big.Int).Set(s.cent)
}

// Wei converts an exact number of cents into wei. This direction is always
// exact — it is multiplication — which is why every amount the service signs
// for originates in cents.
func (s Scale) Wei(c Cents) *big.Int {
	if !s.Valid() {
		return new(big.Int)
	}
	return new(big.Int).Mul(big.NewInt(int64(c)), s.cent)
}

// ToCents floors an observed wei amount to whole cents, which is the one place
// value is rounded and the one place house dust is created. ok is false if the
// amount is too large to be a Cents, which for a real token means something is
// very wrong and the caller should refuse rather than record a clamped number.
func (s Scale) ToCents(wei *big.Int) (c Cents, ok bool) {
	if !s.Valid() || wei == nil || wei.Sign() <= 0 {
		return 0, s.Valid()
	}
	q := new(big.Int).Quo(wei, s.cent)
	if !q.IsInt64() || q.Int64() > math.MaxInt64 {
		return 0, false
	}
	return Cents(q.Int64()), true
}

// Dust is the sub-cent remainder of an amount: what flooring leaves behind.
func (s Scale) Dust(wei *big.Int) *big.Int {
	if !s.Valid() || wei == nil || wei.Sign() <= 0 {
		return new(big.Int)
	}
	return new(big.Int).Mod(wei, s.cent)
}

// Excess is what a wallet holds beyond the cents it owes — the house's claim on
// it. It is negative only if we are insolvent for that wallet, which the caller
// must treat as an alarm rather than as a number to act on.
func (s Scale) Excess(heldWei *big.Int, owed Cents) *big.Int {
	if !s.Valid() {
		return new(big.Int)
	}
	if heldWei == nil {
		heldWei = new(big.Int)
	}
	return new(big.Int).Sub(heldWei, s.Wei(owed))
}
