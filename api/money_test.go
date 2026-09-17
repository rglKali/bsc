package api

import (
	"math/big"
	"strings"
	"testing"
)

// Parse is the input boundary: everything that crosses the wire as an amount
// comes through here, so what it refuses matters more than what it accepts.
func TestParseAcceptsWholeBaseUnits(t *testing.T) {
	cases := map[string]string{
		"0":                     "0",
		"1":                     "1",
		"  42  ":                "42", // surrounding space is trimmed
		"5007000000000000000":   "5007000000000000000",
		strings.Repeat("9", 78): strings.Repeat("9", 78), // a full uint256 width
	}
	for in, want := range cases {
		got, err := parseAmount(in)
		if err != nil {
			t.Fatalf("parseAmount(%q) = %v", in, err)
		}
		if got.String() != want {
			t.Fatalf("parseAmount(%q) = %s, want %s", in, got, want)
		}
	}
}

// A decimal-pointed amount is refused rather than scaled: bsc does not know how
// many decimals the caller had in mind, and guessing would move money (§36).
func TestParseRefusesAnythingButDigits(t *testing.T) {
	for _, in := range []string{
		"", "   ", "1.5", "0.01", "-5", "+5", "1e18", "0x10", "1_000",
		"five", "1,000", strings.Repeat("9", 79),
	} {
		if _, err := parseAmount(in); err == nil {
			t.Fatalf("parseAmount(%q) accepted", in)
		}
	}
}

func TestParsePositiveRefusesZero(t *testing.T) {
	if _, err := parsePositiveAmount("0"); err == nil {
		t.Fatal("parsePositiveAmount(0) accepted")
	}
	got, err := parsePositiveAmount("1")
	if err != nil || got.Sign() != 1 {
		t.Fatalf("parsePositiveAmount(1) = %v, %v", got, err)
	}
}

// Every error names the field's contract, because a caller reading it is about
// to change what it sends.
func TestParseErrorsAreExplicable(t *testing.T) {
	_, err := parseAmount("1.5")
	if err == nil || !strings.Contains(err.Error(), "base units") {
		t.Fatalf("err = %v, want it to say what a valid amount is", err)
	}
}

func TestStringAndOrZeroHandleNil(t *testing.T) {
	if got := amountString(nil); got != "0" {
		t.Fatalf("amountString(nil) = %q, want \"0\"", got)
	}
	if got := orZero(nil); got == nil || got.Sign() != 0 {
		t.Fatalf("orZero(nil) = %v", got)
	}
	v := big.NewInt(7)
	if got := amountString(v); got != "7" {
		t.Fatalf("amountString(7) = %q", got)
	}
}
