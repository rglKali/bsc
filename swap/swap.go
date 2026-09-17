// Package swap encodes the two PancakeSwap-style router calls behind
// `bsc swap`, the attended command an operator runs to turn tokens sitting on
// the master into gas (§44). The running service never trades.
//
// The calls are hand-encoded rather than generated: two functions do not justify
// a build-time toolchain dependency, and writing them out keeps the exact
// calldata visible — which matters for an operation that moves funds to a
// contract address the operator supplies.
//
// The router interface is the Uniswap V2 one, which PancakeSwap V2 and almost
// every fork implement unchanged.
package swap

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// word is the ABI's 32-byte unit.
const word = 32

var (
	selGetAmountsOut = selector("getAmountsOut(uint256,address[])")
	selGetAmountsIn  = selector("getAmountsIn(uint256,address[])")
	selSwapForETH    = selector("swapExactTokensForETH(uint256,uint256,address[],address,uint256)")
	selSwapForExact  = selector("swapTokensForExactETH(uint256,uint256,address[],address,uint256)")
	selWrapped       = selector("WETH()")
)

func selector(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

// PackWrappedNative encodes the router's query for the wrapped-native token it
// routes through.
//
// The swap yields *native* currency — the router unwraps at the end, which is
// what swapExactTokensForETH does — but the path itself is a list of ERC-20s,
// and native currency is not one. So the path has to end at the wrapped token,
// and the safest source for its address is the router itself: asking removes a
// setting that could be set inconsistently with the router in use.
//
// The name is `WETH()` on every Uniswap V2 fork including PancakeSwap, which
// kept the original signature rather than renaming it.
func PackWrappedNative() []byte { return append([]byte(nil), selWrapped...) }

// UnpackAddress decodes a single address return value.
func UnpackAddress(data []byte) (common.Address, error) {
	if len(data) < word {
		return common.Address{}, fmt.Errorf("swap: address response is %d bytes", len(data))
	}
	addr := common.BytesToAddress(data[word-common.AddressLength : word])
	if addr == (common.Address{}) {
		return common.Address{}, errors.New("swap: router returned the zero address")
	}
	return addr, nil
}

// PackGetAmountsOut encodes the router's price query: how much of the last token
// in the path `amountIn` of the first would yield. It is a view call, and its
// answer is what a slippage bound is computed from.
func PackGetAmountsOut(amountIn *big.Int, path []common.Address) []byte {
	out := make([]byte, 0, 4+3*word+len(path)*word)
	out = append(out, selGetAmountsOut...)
	out = appendUint(out, amountIn)
	out = appendUint(out, big.NewInt(2*word)) // offset to the path array
	out = appendPath(out, path)
	return out
}

// PackSwapExactTokensForETH encodes a swap of an exact token amount into native
// currency.
//
// amountOutMin is the caller's protection: the transaction reverts rather than
// filling at a worse price. Passing zero would mean accepting any price at all,
// which on a public mempool is an invitation.
func PackSwapExactTokensForETH(amountIn, amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) []byte {
	out := make([]byte, 0, 4+6*word+len(path)*word)
	out = append(out, selSwapForETH...)
	out = appendUint(out, amountIn)
	out = appendUint(out, amountOutMin)
	out = appendUint(out, big.NewInt(5*word)) // offset to the path array
	out = appendAddr(out, to)
	out = appendUint(out, deadline)
	out = appendPath(out, path)
	return out
}

// PackGetAmountsIn encodes the mirror price query: how much of the *first*
// token in the path is needed to receive `amountOut` of the last.
//
// It exists because an operator refilling gas thinks in the output. "I need a
// tenth of a BNB" is the actual requirement; how many tokens that costs is the
// answer, not the question — and computing it by dividing a getAmountsOut quote
// would be wrong, since the price moves with the size of the trade.
func PackGetAmountsIn(amountOut *big.Int, path []common.Address) []byte {
	out := make([]byte, 0, 4+3*word+len(path)*word)
	out = append(out, selGetAmountsIn...)
	out = appendUint(out, amountOut)
	out = appendUint(out, big.NewInt(2*word)) // offset to the path array
	out = appendPath(out, path)
	return out
}

// PackSwapTokensForExactETH encodes a swap that buys an exact amount of native
// currency, spending no more than amountInMax of the token.
//
// The bound runs the other way from the exact-input call: there the caller is
// protected by a floor on what it receives, here by a ceiling on what it
// spends. Either way the transaction reverts rather than filling at a price the
// caller did not agree to.
//
// Whatever is not spent stays with the caller — the router only pulls what the
// trade actually costs — so a generous ceiling is safe, which is the opposite
// of how an amountOutMin behaves.
func PackSwapTokensForExactETH(amountOut, amountInMax *big.Int, path []common.Address, to common.Address, deadline *big.Int) []byte {
	out := make([]byte, 0, 4+6*word+len(path)*word)
	out = append(out, selSwapForExact...)
	out = appendUint(out, amountOut)
	out = appendUint(out, amountInMax)
	out = appendUint(out, big.NewInt(5*word)) // offset to the path array
	out = appendAddr(out, to)
	out = appendUint(out, deadline)
	out = appendPath(out, path)
	return out
}

// UnpackAmounts decodes a `uint[]` return value, as both router calls produce.
func UnpackAmounts(data []byte) ([]*big.Int, error) {
	if len(data) < 2*word {
		return nil, fmt.Errorf("swap: response is %d bytes, too short for a uint[]", len(data))
	}
	// The first word is the offset to the array; every router returns 0x20, but
	// read it rather than assume it.
	offset := new(big.Int).SetBytes(data[:word])
	if !offset.IsInt64() || offset.Int64() < 0 {
		return nil, errors.New("swap: unreasonable array offset")
	}
	at := int(offset.Int64())
	if at+word > len(data) {
		return nil, errors.New("swap: array offset past the end of the response")
	}
	n := new(big.Int).SetBytes(data[at : at+word])
	if !n.IsInt64() || n.Int64() < 0 || n.Int64() > 64 {
		return nil, fmt.Errorf("swap: unreasonable array length %s", n)
	}
	count := int(n.Int64())
	if at+word+count*word > len(data) {
		return nil, errors.New("swap: array runs past the end of the response")
	}
	amounts := make([]*big.Int, count)
	for i := range amounts {
		start := at + word + i*word
		amounts[i] = new(big.Int).SetBytes(data[start : start+word])
	}
	return amounts, nil
}

// MinOut applies a slippage tolerance in basis points to an expected output.
// It rounds down, so the bound is never looser than asked for.
func MinOut(expected *big.Int, slippageBPS uint32) (*big.Int, error) {
	if expected == nil || expected.Sign() <= 0 {
		return nil, errors.New("swap: expected output must be positive")
	}
	if slippageBPS >= 10_000 {
		// 100% slippage is "accept anything", which is the one setting that
		// makes the bound meaningless.
		return nil, fmt.Errorf("swap: slippage of %d bps would accept any price", slippageBPS)
	}
	keep := new(big.Int).SetUint64(uint64(10_000 - slippageBPS))
	out := new(big.Int).Mul(expected, keep)
	return out.Div(out, big.NewInt(10_000)), nil
}

// MaxIn is MinOut's mirror for an exact-output trade: the most the caller will
// spend to receive the amount it asked for. It rounds up, so the ceiling is
// never tighter than asked for — the opposite rounding from MinOut, and for the
// same reason, which is that slack must always fall on the caller's side.
func MaxIn(expected *big.Int, slippageBPS uint32) (*big.Int, error) {
	if expected == nil || expected.Sign() <= 0 {
		return nil, errors.New("swap: expected input must be positive")
	}
	if slippageBPS >= 10_000 {
		return nil, fmt.Errorf("swap: slippage of %d bps would accept any price", slippageBPS)
	}
	total := new(big.Int).SetUint64(uint64(10_000 + slippageBPS))
	out := new(big.Int).Mul(expected, total)
	// Ceiling division: +9999 before dividing by 10000.
	out.Add(out, big.NewInt(9_999))
	return out.Div(out, big.NewInt(10_000)), nil
}

func appendUint(b []byte, v *big.Int) []byte {
	var buf [word]byte
	if v != nil {
		v.FillBytes(buf[:])
	}
	return append(b, buf[:]...)
}

func appendAddr(b []byte, a common.Address) []byte {
	var buf [word]byte
	copy(buf[word-common.AddressLength:], a.Bytes())
	return append(b, buf[:]...)
}

func appendPath(b []byte, path []common.Address) []byte {
	b = appendUint(b, big.NewInt(int64(len(path))))
	for _, a := range path {
		b = appendAddr(b, a)
	}
	return b
}
