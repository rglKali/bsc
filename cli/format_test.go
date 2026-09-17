package cli

import (
	"math/big"
	"testing"
)

// These helpers exist only at the CLI edge: the service itself never converts
// between a base-unit integer and a decimal string, which is the whole point of
// retiring the cents ledger (§36). A human reading a balance still should not
// have to count eighteen zeroes.
func TestDecRendersBaseUnits(t *testing.T) {
	cases := []struct {
		v      string
		places uint8
		want   string
	}{
		{"0", 18, "0"},
		{"1", 18, "0.000000000000000001"},
		{"1000000000000000000", 18, "1"},
		{"1500000000000000000", 18, "1.5"},
		{"5007000000000000000", 18, "5.007"},
		{"1230000", 6, "1.23"},
		{"-1500000000000000000", 18, "-1.5"},
	}
	for _, c := range cases {
		v, _ := new(big.Int).SetString(c.v, 10)
		if got := dec(v, c.places); got != c.want {
			t.Fatalf("dec(%s, %d) = %q, want %q", c.v, c.places, got, c.want)
		}
	}
	if got := dec(nil, 18); got != "0" {
		t.Fatalf("dec(nil) = %q", got)
	}
}

func TestParseDecScalesToBaseUnits(t *testing.T) {
	cases := []struct {
		in     string
		places uint8
		want   string
	}{
		{"1", 18, "1000000000000000000"},
		{"0.1", 18, "100000000000000000"},
		{"25.5", 18, "25500000000000000000"},
		{" 0.000000000000000001 ", 18, "1"},
		{"1.23", 6, "1230000"},
		{".5", 18, "500000000000000000"},
	}
	for _, c := range cases {
		got, err := parseDec(c.in, c.places)
		if err != nil {
			t.Fatalf("parseDec(%q): %v", c.in, err)
		}
		if got.String() != c.want {
			t.Fatalf("parseDec(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// More fraction digits than the token has are refused rather than truncated:
// silently dropping a digit changes the amount being traded.
func TestParseDecRefusesTooMuchPrecision(t *testing.T) {
	if _, err := parseDec("1.1234567", 6); err == nil {
		t.Fatal("parseDec accepted more decimals than the token has")
	}
	for _, bad := range []string{"", "0", "0.0", "-1", "abc", "1.2.3"} {
		if _, err := parseDec(bad, 18); err == nil {
			t.Fatalf("parseDec(%q) accepted", bad)
		}
	}
}

// dec and parseDec are inverses over the values a human types, which is what
// makes the quote an operator confirms the same number that gets signed.
func TestDecAndParseDecRoundTrip(t *testing.T) {
	for _, in := range []string{"1", "0.1", "25.5", "0.000000000000000001", "1234.5678"} {
		v, err := parseDec(in, 18)
		if err != nil {
			t.Fatalf("parseDec(%q): %v", in, err)
		}
		if got := dec(v, 18); got != in {
			t.Fatalf("round trip of %q gave %q", in, got)
		}
	}
}

func TestBumpedAppliesTheMultiplier(t *testing.T) {
	if got := bumped(big.NewInt(100), 1.1); got.Int64() != 110 {
		t.Fatalf("bumped = %s, want 110", got)
	}
	// A multiplier of 1 or less is a no-op rather than a reduction.
	if got := bumped(big.NewInt(100), 1); got.Int64() != 100 {
		t.Fatalf("bumped = %s, want 100", got)
	}
	if got := bumped(nil, 1.1); got != nil {
		t.Fatalf("bumped(nil) = %v", got)
	}
}

func TestMaxUint256IsTheFullWidth(t *testing.T) {
	if got := maxUint256().BitLen(); got != 256 {
		t.Fatalf("bit length = %d, want 256", got)
	}
}
