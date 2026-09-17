package cli

import (
	"math/big"
	"testing"
)

// belowFloor is the guard that makes `bsc swap` safe to run on a timer: the
// automation lives in cron rather than in the service, so the check has to be
// cheap and has to fail towards doing nothing (§44).
func TestBelowFloor(t *testing.T) {
	floor := big.NewInt(50_000_000_000_000_000) // 0.05

	cases := map[string]struct {
		native *big.Int
		floor  *big.Int
		want   bool
	}{
		"under":           {big.NewInt(1), floor, true},
		"exactly at":      {new(big.Int).Set(floor), floor, false},
		"over":            {big.NewInt(60_000_000_000_000_000), floor, false},
		"no floor set":    {big.NewInt(1), nil, false},
		"zero floor":      {big.NewInt(1), big.NewInt(0), false},
		"empty, no floor": {new(big.Int), nil, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := &masterCtx{cfg: cfgWithFloor(c.floor)}
			got := m.wouldTrade(c.native)
			if got != c.want {
				t.Fatalf("wouldTrade(%s, floor %v) = %v, want %v", c.native, c.floor, got, c.want)
			}
		})
	}
}

// An unset floor reads as "fine", never as "always trade". Getting that
// backwards would mean a cron line trading on every run.
func TestNoFloorNeverTrades(t *testing.T) {
	m := &masterCtx{cfg: cfgWithFloor(nil)}
	if m.wouldTrade(new(big.Int)) {
		t.Fatal("an empty master with no configured floor was judged to need a trade")
	}
}
