package cli

import (
	"bsc/config"
	"bufio"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/spf13/cobra"
)

// Rendering helpers for the operator commands.
//
// These exist only at the CLI edge. The service itself never converts between
// a base-unit integer and a decimal string — that was the whole point of
// retiring the cents ledger (§36) — but a human reading a balance should not
// have to count eighteen zeroes.

// dec renders a base-unit integer as a decimal string with `places` fraction
// digits, trailing zeroes trimmed. It is for display only; nothing parses it
// back.
func dec(v *big.Int, places uint8) string {
	if v == nil {
		return "0"
	}
	neg := v.Sign() < 0
	abs := new(big.Int).Abs(v)
	whole, frac := new(big.Int).QuoRem(abs, pow10(places), new(big.Int))
	out := whole.String()
	if frac.Sign() != 0 {
		digits := fmt.Sprintf("%0*s", places, frac.String())
		out += "." + strings.TrimRight(digits, "0")
	}
	if neg {
		out = "-" + out
	}
	return out
}

// parseDec reads a decimal amount a human typed and scales it to base units.
// It refuses more fraction digits than the token has, rather than truncating:
// silently dropping a digit changes the amount being traded.
func parseDec(s string, places uint8) (*big.Int, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > int(places) {
		return nil, fmt.Errorf("%q has more than %d decimal places", s, places)
	}
	digits := whole + frac + strings.Repeat("0", int(places)-len(frac))
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("%q is not a positive decimal amount", s)
	}
	if v.Sign() == 0 {
		return nil, fmt.Errorf("amount must be greater than zero")
	}
	return v, nil
}

func pow10(n uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func maxUint256() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
}

// bumped applies the configured gas-price multiplier. Estimates go stale
// between the quote and the broadcast, and a transaction that never mines is
// worse than one that cost a fraction more.
func bumped(v *big.Int, mul float64) *big.Int {
	if v == nil || mul <= 1 {
		return v
	}
	f := new(big.Float).Mul(new(big.Float).SetInt(v), big.NewFloat(mul))
	out, _ := f.Int(nil)
	return out
}

func callMsg(from, to common.Address, value *big.Int, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{From: from, To: &to, Value: value, Data: data}
}

// confirm asks before signing. A trade is the one operator action whose outcome
// is a price rather than a yes or no, so it gets the one prompt in this CLI.
func confirm(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	r := bufio.NewReader(cmd.InOrStdin())
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// cfgWithFloor builds the minimal configuration the gas-floor check needs. It
// lives here rather than in the test so the field it reaches for is named in
// one place.
func cfgWithFloor(floor *big.Int) config.Config {
	return config.Config{GasFloor: floor}
}
