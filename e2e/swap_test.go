//go:build e2e

package e2e

import (
	"math/big"
	"testing"
	"time"

	"bsc/swap"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestOperatorSwap exercises the path `bsc swap` takes, against a real router
// on a real chain.
//
// It is the one operation whose outcome is a price rather than a yes or no,
// which is exactly why it is no longer something the service starts on its own
// (§38) — and exactly why a simulator proves nothing about it. A fake router
// can only confirm our own assumptions about calldata; only a real one can say
// whether the quote, the slippage bound and the exact-output call actually
// behave the way the encoding assumes.
//
// This drives the same sequence the command does — quote, bound, approve, swap,
// confirm — rather than shelling out, so a failure points at a line of Go.
func TestOperatorSwap(t *testing.T) {
	h := setup(t, needs{swap: true})

	wrapped := h.wrappedNative()
	path := []common.Address{h.token, wrapped}
	sell := h.swapAmount

	nativeBefore, err := h.chain.BalanceBNB(h.ctx, h.master)
	if err != nil {
		t.Fatalf("native balance: %v", err)
	}
	tokensBefore := h.tokenBalance(h.master)
	if tokensBefore.Cmp(sell) < 0 {
		t.Skipf("master holds %s tokens, which is less than the %s this test trades",
			fmtToken(tokensBefore), fmtToken(sell))
	}

	t.Run("the router quotes both directions", func(t *testing.T) {
		// getAmountsOut answers "what would this fetch"; getAmountsIn answers
		// "what would that cost". A caller refilling gas thinks in the second,
		// and computing it by dividing the first would be wrong — the price
		// moves with the size of the trade.
		out := h.quote(swap.PackGetAmountsOut(sell, path))
		if out[len(out)-1].Sign() <= 0 {
			t.Fatalf("getAmountsOut returned %v", out)
		}
		want := out[len(out)-1]

		in := h.quote(swap.PackGetAmountsIn(want, path))
		if in[0].Sign() <= 0 {
			t.Fatalf("getAmountsIn returned %v", in)
		}
		// Round-tripping a quote should land within a whisker of where it
		// started. A wide gap means the path or the encoding is wrong, not that
		// the pool moved.
		diff := new(big.Int).Sub(in[0], sell)
		diff.Abs(diff)
		tolerance := new(big.Int).Div(sell, big.NewInt(20)) // 5%
		if diff.Cmp(tolerance) > 0 {
			t.Fatalf("round-tripped quote: sold %s, getAmountsIn says %s costs it",
				fmtToken(sell), fmtToken(in[0]))
		}
		t.Logf("%s tokens ⇄ %s native", fmtToken(sell), fmtToken(want))
	})

	t.Run("an exact-input trade lands inside its floor", func(t *testing.T) {
		out := h.quote(swap.PackGetAmountsOut(sell, path))
		expected := out[len(out)-1]
		minOut, err := swap.MinOut(expected, 300) // testnet pools are thin
		if err != nil {
			t.Fatalf("MinOut: %v", err)
		}

		h.approveRouter(sell)

		deadline := big.NewInt(time.Now().Add(5 * time.Minute).Unix())
		data := swap.PackSwapExactTokensForETH(sell, minOut, path, h.master, deadline)
		hash := h.signAndSend(h.masterKey, h.router, new(big.Int), data)
		t.Logf("swap %s", hash.Hex())
		h.awaitReceipt(hash)

		// Exactly the input left, and at least the floor arrived. Gas makes the
		// native side inexact, so the assertion is the direction and the bound,
		// not an equality.
		tokensAfter := h.tokenBalance(h.master)
		spent := new(big.Int).Sub(tokensBefore, tokensAfter)
		if spent.Cmp(sell) != 0 {
			t.Fatalf("spent %s tokens, want exactly %s", fmtToken(spent), fmtToken(sell))
		}
		nativeAfter, err := h.chain.BalanceBNB(h.ctx, h.master)
		if err != nil {
			t.Fatalf("native balance: %v", err)
		}
		if nativeAfter.Cmp(nativeBefore) <= 0 {
			t.Fatalf("native went from %s to %s — the trade did not pay for its own gas",
				nativeBefore, nativeAfter)
		}
		t.Logf("native %s → %s (floor was %s)", nativeBefore, nativeAfter, minOut)
	})
}

// wrappedNative asks the router for its own wrapped token, which is what
// removes a setting that could be configured inconsistently with it.
func (h *harness) wrappedNative() common.Address {
	h.t.Helper()
	raw, err := h.chain.Call(h.ctx, h.router, swap.PackWrappedNative())
	if err != nil {
		h.t.Fatalf("router WETH(): %v", err)
	}
	addr, err := swap.UnpackAddress(raw)
	if err != nil {
		h.t.Fatalf("decode WETH(): %v", err)
	}
	return addr
}

func (h *harness) quote(data []byte) []*big.Int {
	h.t.Helper()
	raw, err := h.chain.Call(h.ctx, h.router, data)
	if err != nil {
		h.t.Fatalf("router quote: %v", err)
	}
	amounts, err := swap.UnpackAmounts(raw)
	if err != nil {
		h.t.Fatalf("decode amounts: %v", err)
	}
	if len(amounts) < 2 {
		h.t.Fatalf("router returned %d amounts", len(amounts))
	}
	return amounts
}

// approveRouter grants the allowance the trade needs, skipping when one is
// already in place — the same shape the command uses.
func (h *harness) approveRouter(need *big.Int) {
	h.t.Helper()
	abi := usdt.NewUsdt()
	raw, err := h.chain.Call(h.ctx, h.token, abi.PackAllowance(h.master, h.router))
	if err != nil {
		h.t.Fatalf("allowance: %v", err)
	}
	allowance, err := abi.UnpackAllowance(raw)
	if err != nil {
		h.t.Fatalf("decode allowance: %v", err)
	}
	if allowance.Cmp(need) >= 0 {
		return
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	hash := h.signAndSend(h.masterKey, h.token, new(big.Int), abi.PackApprove(h.router, max))
	h.t.Logf("approve %s", hash.Hex())
	h.awaitReceipt(hash)
}

// awaitReceipt blocks until a transaction is mined, failing if it reverted.
func (h *harness) awaitReceipt(hash common.Hash) {
	h.t.Helper()
	ok := h.waitFor("receipt for "+hash.Hex(), func() bool {
		rc, err := h.chain.Receipt(h.ctx, hash)
		if err != nil || rc == nil {
			return false
		}
		if rc.Status != types.ReceiptStatusSuccessful {
			h.t.Fatalf("transaction %s reverted", hash.Hex())
		}
		return true
	})
	if !ok {
		h.t.Fatalf("gave up waiting for %s", hash.Hex())
	}
}
