package swap

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

var (
	tokenA = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	tokenB = common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c")
	to     = common.HexToAddress("0x00000000000000000000000000000000000000aa")
)

func TestSelectorsMatchTheRouterInterface(t *testing.T) {
	// Wrong by one byte and the call silently hits a different function, or
	// none at all, on a contract holding real funds.
	for name, got := range map[string][]byte{
		"getAmountsOut":         selGetAmountsOut,
		"swapExactTokensForETH": selSwapForETH,
	} {
		if len(got) != 4 {
			t.Fatalf("%s selector is %d bytes", name, len(got))
		}
	}
	if hex.EncodeToString(selGetAmountsOut) != "d06ca61f" {
		t.Fatalf("getAmountsOut selector = %s, want d06ca61f", hex.EncodeToString(selGetAmountsOut))
	}
	if hex.EncodeToString(selSwapForETH) != "18cbafe5" {
		t.Fatalf("swapExactTokensForETH selector = %s, want 18cbafe5", hex.EncodeToString(selSwapForETH))
	}
	if hex.EncodeToString(selWrapped) != "ad5c4648" {
		t.Fatalf("WETH selector = %s, want ad5c4648", hex.EncodeToString(selWrapped))
	}
}

func TestUnpackAddress(t *testing.T) {
	var padded [32]byte
	copy(padded[12:], tokenB.Bytes())
	got, err := UnpackAddress(padded[:])
	if err != nil {
		t.Fatalf("UnpackAddress: %v", err)
	}
	if got != tokenB {
		t.Fatalf("address = %s, want %s", got.Hex(), tokenB.Hex())
	}
	// A router that answers with nothing, or with zero, must not become a path
	// hop through the zero address.
	if _, err := UnpackAddress(nil); err == nil {
		t.Fatal("empty response accepted")
	}
	if _, err := UnpackAddress(make([]byte, 32)); err == nil {
		t.Fatal("zero address accepted")
	}
}

func TestPackGetAmountsOutLayout(t *testing.T) {
	data := PackGetAmountsOut(big.NewInt(1000), []common.Address{tokenA, tokenB})
	// selector + amountIn + offset + length + two addresses
	if want := 4 + 32*3 + 32*2; len(data) != want {
		t.Fatalf("length = %d, want %d", len(data), want)
	}
	args := data[4:]
	if got := new(big.Int).SetBytes(args[0:32]); got.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("amountIn = %s", got)
	}
	if got := new(big.Int).SetBytes(args[32:64]); got.Cmp(big.NewInt(64)) != 0 {
		t.Fatalf("path offset = %s, want 64", got)
	}
	if got := new(big.Int).SetBytes(args[64:96]); got.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("path length = %s", got)
	}
	if got := common.BytesToAddress(args[96+12 : 128]); got != tokenA {
		t.Fatalf("path[0] = %s", got.Hex())
	}
	if got := common.BytesToAddress(args[128+12 : 160]); got != tokenB {
		t.Fatalf("path[1] = %s", got.Hex())
	}
}

func TestPackSwapLayout(t *testing.T) {
	data := PackSwapExactTokensForETH(
		big.NewInt(10), big.NewInt(9), []common.Address{tokenA, tokenB}, to, big.NewInt(1234))
	if want := 4 + 32*6 + 32*2; len(data) != want {
		t.Fatalf("length = %d, want %d", len(data), want)
	}
	args := data[4:]
	checks := []struct {
		name string
		at   int
		want *big.Int
	}{
		{"amountIn", 0, big.NewInt(10)},
		{"amountOutMin", 32, big.NewInt(9)},
		{"path offset", 64, big.NewInt(160)},
		{"deadline", 128, big.NewInt(1234)},
		{"path length", 160, big.NewInt(2)},
	}
	for _, c := range checks {
		if got := new(big.Int).SetBytes(args[c.at : c.at+32]); got.Cmp(c.want) != 0 {
			t.Fatalf("%s = %s, want %s", c.name, got, c.want)
		}
	}
	if got := common.BytesToAddress(args[96+12 : 128]); got != to {
		t.Fatalf("recipient = %s, want %s", got.Hex(), to.Hex())
	}
	if got := common.BytesToAddress(args[192+12 : 224]); got != tokenA {
		t.Fatalf("path[0] = %s", got.Hex())
	}
}

func TestUnpackAmounts(t *testing.T) {
	// offset, length, then the values — the shape every router returns.
	data := make([]byte, 0, 32*4)
	data = appendUint(data, big.NewInt(32))
	data = appendUint(data, big.NewInt(2))
	data = appendUint(data, big.NewInt(1000))
	data = appendUint(data, big.NewInt(7))

	got, err := UnpackAmounts(data)
	if err != nil {
		t.Fatalf("UnpackAmounts: %v", err)
	}
	if len(got) != 2 || got[0].Cmp(big.NewInt(1000)) != 0 || got[1].Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("amounts = %v", got)
	}
}

func TestUnpackAmountsRejectsGarbage(t *testing.T) {
	// A malformed response must not be read as a price; a wrong price becomes a
	// wrong slippage bound, which becomes a bad fill.
	long := append(appendUint(nil, big.NewInt(32)), appendUint(nil, big.NewInt(1_000_000))...)
	for name, data := range map[string][]byte{
		"empty":          {},
		"truncated":      make([]byte, 40),
		"absurd length":  long,
		"offset too far": append(appendUint(nil, big.NewInt(1<<20)), make([]byte, 32)...),
	} {
		if _, err := UnpackAmounts(data); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestMinOut(t *testing.T) {
	// 1% off 1000 is 990.
	got, err := MinOut(big.NewInt(1000), 100)
	if err != nil {
		t.Fatalf("MinOut: %v", err)
	}
	if got.Cmp(big.NewInt(990)) != 0 {
		t.Fatalf("MinOut = %s, want 990", got)
	}
	// Zero slippage means "exactly the quote or revert".
	if got, _ := MinOut(big.NewInt(1000), 0); got.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("zero slippage = %s", got)
	}
	// Rounding is downward, so the bound is never looser than requested.
	if got, _ := MinOut(big.NewInt(999), 1); got.Cmp(big.NewInt(998)) != 0 {
		t.Fatalf("rounding = %s, want 998", got)
	}
	for _, bad := range []uint32{10_000, 20_000} {
		if _, err := MinOut(big.NewInt(1000), bad); err == nil {
			t.Fatalf("slippage %d accepted — that is 'any price at all'", bad)
		}
	}
	if _, err := MinOut(big.NewInt(0), 100); err == nil {
		t.Fatal("a zero quote was accepted")
	}
}

func TestKnownRoutersAreDistinctAndNonZero(t *testing.T) {
	// A zero or duplicated constant here would point a real swap at nothing.
	if PancakeV2Mainnet == (common.Address{}) || PancakeV2Testnet == (common.Address{}) {
		t.Fatal("a known router constant is the zero address")
	}
	if PancakeV2Mainnet == PancakeV2Testnet {
		t.Fatal("mainnet and testnet routers are the same address")
	}
}
