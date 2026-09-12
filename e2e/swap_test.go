//go:build e2e

package e2e

import (
	"math/big"
	"testing"
	"time"

	"bsc/engine"
	"bsc/store"
	"bsc/swap"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// quote asks the router what the swap amount is currently worth, which is also
// how we learn whether the pool exists at all.
func (h *harness) quote(amount *big.Int) (*big.Int, error) {
	h.t.Helper()
	out, err := h.chain.Call(h.ctx, h.router, swap.PackWrappedNative())
	if err != nil {
		return nil, err
	}
	wrapped, err := swap.UnpackAddress(out)
	if err != nil {
		return nil, err
	}
	path := []common.Address{h.token, wrapped}
	quoted, err := h.chain.Call(h.ctx, h.router, swap.PackGetAmountsOut(amount, path))
	if err != nil {
		return nil, err
	}
	amounts, err := swap.UnpackAmounts(quoted)
	if err != nil {
		return nil, err
	}
	return amounts[len(amounts)-1], nil
}

// TestGasTopUpSwap exercises the one operation whose outcome is a price rather
// than a yes or no: the master trading collected fees back into native gas.
//
// It is the hardest flow to fake convincingly — a simulator can only confirm our
// own assumptions about a router — so it matters most that it runs here.
func TestGasTopUpSwap(t *testing.T) {
	h := setup(t, needs{swap: true})

	// A testnet pool may simply not exist. That is an environment problem, not
	// a failure of the code, so say so plainly rather than failing.
	expected, err := h.quote(h.swapAmount)
	if err != nil {
		t.Skipf("router %s cannot quote %s of %s → native: %v\n"+
			"There is probably no pool for this pair on this chain. "+
			"Set E2E_SWAP_ROUTER or E2E_TOKEN_ADDRESS to a pair that has liquidity.",
			h.router.Hex(), fmtToken(h.swapAmount), h.token.Hex(), err)
	}
	if expected == nil || expected.Sign() == 0 {
		t.Skipf("router quoted zero native for %s — no liquidity to swap against", fmtToken(h.swapAmount))
	}
	t.Logf("router quotes %s native for %s tokens", fmtToken(expected), fmtToken(h.swapAmount))

	// The master trades its own collected fees, so give it some to trade. On a
	// live service these arrive from withdrawals; here the funder stands in.
	//
	// Wait for the balance to *rise*, not merely to clear the swap amount: the
	// master is the wallet that funds everything, so it already holds tokens and
	// "at least one" is true before the transfer has mined. Reading the balance
	// then would snapshot it mid-flight, and the token that arrived afterwards
	// would silently cancel out the one the swap spends.
	want := new(big.Int).Add(h.tokenBalance(h.master), h.swapAmount)
	h.sendTokens(h.master, h.swapAmount)
	h.pump("master funded with tokens to trade", func() bool {
		return h.tokenBalance(h.master).Cmp(want) >= 0
	})

	beforeNative, err := h.chain.BalanceBNB(h.ctx, h.master)
	if err != nil {
		t.Fatalf("master balance: %v", err)
	}
	beforeTokens := h.tokenBalance(h.master)

	// Record the master as a wallet and put its balance where the rule can see
	// it, then set a floor above the current balance so a top-up is due.
	master := h.masterWallet(beforeTokens)
	cfg := engine.Config{
		SwapEnabled:  true,
		SwapAmount:   h.swapAmount,
		GasFloor:     new(big.Int).Add(beforeNative, big.NewInt(1)),
		SwapCooldown: time.Minute,
	}
	var started bool
	if err := h.store.Update(func(tx *store.Tx) error {
		var err error
		started, err = engine.EvaluateGas(tx, cfg, master, beforeNative, time.Now())
		return err
	}); err != nil {
		t.Fatalf("EvaluateGas: %v", err)
	}
	if !started {
		t.Fatalf("no top-up started: native %s below floor %s, holding %s tokens",
			beforeNative, cfg.GasFloor, fmtToken(beforeTokens))
	}

	// Two transactions: the router allowance, then the trade itself.
	h.pump("swap completed", func() bool {
		n := 0
		_ = h.store.View(func(tx *store.Tx) error {
			return tx.EachFlow(func(f store.Flow) error {
				if f.Kind == store.FlowGasTopUp {
					n++
				}
				return nil
			})
		})
		return n == 0
	})

	afterNative, err := h.chain.BalanceBNB(h.ctx, h.master)
	if err != nil {
		t.Fatalf("master balance: %v", err)
	}
	afterTokens := h.tokenBalance(h.master)

	// Tokens went out.
	spent := new(big.Int).Sub(beforeTokens, afterTokens)
	if spent.Cmp(h.swapAmount) != 0 {
		t.Fatalf("traded %s tokens, want exactly the configured %s",
			fmtToken(spent), fmtToken(h.swapAmount))
	}

	// Native came back. The master also paid gas for the approve and the swap,
	// so the net change can be smaller than the quote — what matters is that
	// the proceeds landed, not that the balance rose by the full amount.
	if afterNative.Cmp(beforeNative) <= 0 {
		t.Fatalf("native balance did not rise: %s → %s (gas may have exceeded a very small trade)",
			beforeNative, afterNative)
	}
	t.Logf("swapped %s tokens for native: balance %s → %s",
		fmtToken(h.swapAmount), beforeNative, afterNative)

	// And the audit still agrees with the chain afterwards.
	rep, err := h.audit()
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("audit findings after the swap: %v", rep.Findings)
	}
}

// masterWallet records the master, as the running service does at startup.
func (h *harness) masterWallet(balance *big.Int) store.Wallet {
	h.t.Helper()
	var out store.Wallet
	if err := h.store.Update(func(tx *store.Tx) error {
		existing, ok, err := tx.WalletByAddress(h.master)
		if err != nil {
			return err
		}
		if ok {
			out = existing
			_, err = tx.MutateWallet(out.ID, func(w *store.Wallet) error {
				w.Balance = balance
				return nil
			})
			out.Balance = balance
			return err
		}
		out = store.Wallet{
			ID:   uuid.NewSHA1(uuid.NameSpaceOID, h.master.Bytes()),
			Kind: store.KindMaster, Address: h.master,
			Balance: balance, CreatedAt: time.Now().UTC(),
		}
		return tx.PutWallet(out)
	}); err != nil {
		h.t.Fatalf("record master wallet: %v", err)
	}
	return out
}
