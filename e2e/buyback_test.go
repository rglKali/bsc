//go:build e2e

package e2e

import (
	"math/big"
	"time"

	"bsc/swap"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The router's buy side, encoded here rather than in `swap`: the service sells
// collected fees for gas and never trades the other way, so `swap` stays exactly
// as wide as bsc's own needs and this stays what it is — scaffolding that puts
// the run's tokens back.
var (
	selGetAmountsIn  = crypto.Keccak256([]byte("getAmountsIn(uint256,address[])"))[:4]
	selBuyExactOut   = crypto.Keccak256([]byte("swapETHForExactTokens(uint256,address[],address,uint256)"))[:4]
	tokenDust        = new(big.Int).Div(whole(1), big.NewInt(1000))
	nativeFloor      = big.NewInt(20_000_000_000_000_000) // 0.02, kept back for the next run's gas
	buybackSlippage  = big.NewInt(10_300)                 // 3%, matching the sell side
	buybackSlipScale = big.NewInt(10_000)
)

// packGetAmountsIn asks what native input a desired token output would cost —
// the mirror of getAmountsOut, and the quote an exact-output swap is bounded by.
func packGetAmountsIn(amountOut *big.Int, path []common.Address) []byte {
	out := append([]byte(nil), selGetAmountsIn...)
	out = abiUint(out, amountOut)
	out = abiUint(out, big.NewInt(2*32)) // offset to the path array
	return abiPath(out, path)
}

// packBuyExactTokens encodes a swap that buys an exact number of tokens, paying
// with the transaction's own native value.
//
// Exact-output is what makes the balance land back on the number it started at:
// the router takes only what the trade needs and refunds the rest, so the bound
// can be generous without overspending.
func packBuyExactTokens(amountOut *big.Int, path []common.Address, to common.Address, deadline *big.Int) []byte {
	out := append([]byte(nil), selBuyExactOut...)
	out = abiUint(out, amountOut)
	out = abiUint(out, big.NewInt(4*32)) // offset to the path array
	out = abiAddr(out, to)
	out = abiUint(out, deadline)
	return abiPath(out, path)
}

func abiUint(b []byte, v *big.Int) []byte {
	var buf [32]byte
	if v != nil {
		v.FillBytes(buf[:])
	}
	return append(b, buf[:]...)
}

func abiAddr(b []byte, a common.Address) []byte {
	var buf [32]byte
	copy(buf[32-common.AddressLength:], a.Bytes())
	return append(b, buf[:]...)
}

func abiPath(b []byte, path []common.Address) []byte {
	b = abiUint(b, big.NewInt(int64(len(path))))
	for _, a := range path {
		b = abiAddr(b, a)
	}
	return b
}

// restoreTokens buys back whatever the run consumed, so the master ends holding
// what it started with.
//
// Only the gas top-up actually spends tokens — every other flow is a closed loop
// that returns them — and it does not lose them so much as convert them into
// native currency. Trading that back is simply the conversion in reverse, and it
// is what makes the suite repeatable: without it the token balance ratchets down
// by one per run until a preflight fails and you are back at a faucet.
//
// It is expressed as "restore the starting balance" rather than "undo the swap"
// so that it also covers tokens a failed run stranded somewhere unrecoverable,
// and it is bounded by nativeFloor so it can never eat the gas the next run needs.
func (h *harness) restoreTokens() {
	h.t.Helper()

	held := h.tokenBalance(h.master)
	deficit := new(big.Int).Sub(h.startTokens, held)
	if deficit.Cmp(tokenDust) < 0 {
		return // the run was a closed loop, as most of them are
	}

	// The path is the sell side reversed, and the wrapped token comes from the
	// router for the same reason it does there: it cannot then disagree with it.
	out, err := h.chain.Call(h.ctx, h.router, swap.PackWrappedNative())
	if err != nil {
		h.t.Logf("recovery: router %s: %v — leaving the master %s short",
			h.router.Hex(), err, fmtToken(deficit))
		return
	}
	wrapped, err := swap.UnpackAddress(out)
	if err != nil {
		h.t.Logf("recovery: wrapped native: %v", err)
		return
	}
	path := []common.Address{wrapped, h.token}

	quoted, err := h.chain.Call(h.ctx, h.router, packGetAmountsIn(deficit, path))
	if err != nil {
		h.t.Logf("recovery: cannot quote %s back: %v — the pool may be one-sided",
			fmtToken(deficit), err)
		return
	}
	amounts, err := swap.UnpackAmounts(quoted)
	if err != nil || len(amounts) == 0 || amounts[0].Sign() == 0 {
		h.t.Logf("recovery: router quoted nothing to buy %s back (%v)", fmtToken(deficit), err)
		return
	}
	maxIn := new(big.Int).Mul(amounts[0], buybackSlippage)
	maxIn.Div(maxIn, buybackSlipScale)

	// Never spend the next run's gas on tokens. Being short a token is an
	// inconvenience; being short gas means nothing runs at all.
	native, err := h.chain.BalanceBNB(h.ctx, h.master)
	if err != nil {
		h.t.Logf("recovery: master balance: %v", err)
		return
	}
	budget := new(big.Int).Sub(native, nativeFloor)
	if maxIn.Cmp(budget) > 0 {
		h.t.Logf("recovery: buying %s back needs up to %s native but the master holds %s and keeps %s for gas — leaving it short",
			fmtToken(deficit), maxIn, native, nativeFloor)
		return
	}

	h.t.Logf("buying back %s tokens for up to %s native (quoted %s)",
		fmtToken(deficit), maxIn, amounts[0])
	deadline := big.NewInt(time.Now().Add(10 * time.Minute).Unix())
	h.signAndSend(h.masterKey, h.router, maxIn, packBuyExactTokens(deficit, path, h.master, deadline))

	h.waitFor("tokens bought back", func() bool {
		return h.tokenBalance(h.master).Cmp(h.startTokens) >= 0
	})
}
