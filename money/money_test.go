package money

import (
	"errors"
	"math/big"
	"testing"
)

func scale(t *testing.T, decimals uint8) Scale {
	t.Helper()
	s, err := NewScale(decimals)
	if err != nil {
		t.Fatalf("NewScale(%d): %v", decimals, err)
	}
	return s
}

func TestScaleFollowsTheTokensDecimals(t *testing.T) {
	// A cent is a different number of wei on every token, which is exactly why
	// it is read from the contract rather than configured.
	for _, tc := range []struct {
		decimals uint8
		centWei  int64
	}{{2, 1}, {6, 10_000}, {18, 10_000_000_000_000_000}} {
		if got := scale(t, tc.decimals).CentWei(); got.Cmp(big.NewInt(tc.centWei)) != 0 {
			t.Fatalf("%d decimals: a cent is %s wei, want %d", tc.decimals, got, tc.centWei)
		}
	}
}

func TestATokenTooCoarseForCentsIsRefused(t *testing.T) {
	// One decimal cannot represent a cent, and every amount in the service is a
	// number of cents. Failing here beats being wrong by a factor of ten
	// everywhere.
	for _, d := range []uint8{0, 1} {
		if _, err := NewScale(d); !errors.Is(err, ErrDecimals) {
			t.Fatalf("%d decimals accepted: %v", d, err)
		}
	}
}

func TestZeroScaleIsUnusable(t *testing.T) {
	// A component that was never told the token's decimals must be detectably
	// broken rather than quietly valuing a cent at a wei.
	var s Scale
	if s.Valid() {
		t.Fatal("the zero Scale reports itself usable")
	}
}

func TestFlooringAlwaysRoundsTowardsTheHouse(t *testing.T) {
	s := scale(t, 18)
	cent := s.CentWei()

	for _, tc := range []struct {
		wei   *big.Int
		cents Cents
		dust  int64
	}{
		{new(big.Int).Mul(cent, big.NewInt(1000)), 1000, 0},                                  // exact
		{new(big.Int).Add(new(big.Int).Mul(cent, big.NewInt(1000)), big.NewInt(1)), 1000, 1}, // a wei over
		{new(big.Int).Sub(cent, big.NewInt(1)), 0, 0},                                        // sub-cent: nothing credited
	} {
		got, ok := s.ToCents(tc.wei)
		if !ok {
			t.Fatalf("%s: not representable", tc.wei)
		}
		if got != tc.cents {
			t.Fatalf("%s wei = %d cents, want %d", tc.wei, got, tc.cents)
		}
		// Flooring never credits more than arrived: the remainder is the
		// house's, and it is never negative.
		back := s.Wei(got)
		if back.Cmp(tc.wei) > 0 {
			t.Fatalf("credited %s wei for a %s transfer", back, tc.wei)
		}
	}

	// A sub-cent transfer is worth nothing to the app but is not lost: it stays
	// in the wallet as dust.
	if d := s.Dust(new(big.Int).Sub(cent, big.NewInt(1))); d.Cmp(new(big.Int).Sub(cent, big.NewInt(1))) != 0 {
		t.Fatalf("dust = %s, want the whole sub-cent amount", d)
	}
}

func TestAnAmountTooLargeToBeCentsIsRefused(t *testing.T) {
	// Impossible for a real token, and a clamped number in the books would be
	// far worse than a refusal.
	s := scale(t, 18)
	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(40), nil)
	if _, ok := s.ToCents(huge); ok {
		t.Fatal("an unrepresentable amount was accepted")
	}
}

func TestExcessIsWhatTheHouseHolds(t *testing.T) {
	s := scale(t, 18)
	held := s.Wei(1500) // the wallet holds $15
	if got := s.Excess(held, 1000); got.Cmp(s.Wei(500)) != 0 {
		t.Fatalf("excess = %s, want the $5 nobody is owed", got)
	}
	if got := s.Excess(held, 1500); got.Sign() != 0 {
		t.Fatalf("excess = %s when the wallet holds exactly what is owed", got)
	}
	// Negative means insolvent, and must be reported as such rather than
	// clamped to zero — a caller has to be able to tell the difference.
	if got := s.Excess(held, 2000); got.Sign() >= 0 {
		t.Fatalf("excess = %s while short, want a negative figure", got)
	}
}

func TestParseTakesWholeCentsOnly(t *testing.T) {
	for _, in := range []string{"0", "1", "150", "9007199254740993"} {
		if _, err := Parse(in); err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
	}
	// A decimal point means the caller was thinking in tokens, and guessing
	// which they meant is worse than refusing.
	for _, in := range []string{"", "1.5", "1,5", "-1", "abc", "0x10", " "} {
		if _, err := Parse(in); !errors.Is(err, ErrBadAmount) {
			t.Fatalf("Parse(%q) = %v, want ErrBadAmount", in, err)
		}
	}
}

func TestTokensRendersForHumansOnly(t *testing.T) {
	for _, tc := range []struct {
		in   Cents
		want string
	}{{0, "0.00"}, {5, "0.05"}, {150, "1.50"}, {100_000, "1000.00"}, {-150, "-1.50"}} {
		if got := tc.in.Tokens(); got != tc.want {
			t.Fatalf("Cents(%d).Tokens() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
