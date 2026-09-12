//go:build e2e

package e2e

import (
	"math/big"
	"os"
	"strings"
	"testing"

	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
)

// envRecover names addresses to pull tokens back from, comma-separated.
const envRecover = "E2E_RECOVER"

// TestRecoverStrandedTokens is a cleanup tool, not a test of the service.
//
// A run that fails partway leaves tokens in wallets it derived, and once its
// temporary database is gone so are their ids — but that does not matter: the
// master's allowance is granted on an *address*, so it can still pull the
// balance back knowing nothing else. Addresses are printed in the run log.
//
//	E2E_RECOVER=0xabc…,0xdef… task e2e -- -run TestRecoverStrandedTokens
//
// It is skipped unless asked for.
func TestRecoverStrandedTokens(t *testing.T) {
	list := os.Getenv(envRecover)
	if list == "" {
		t.Skipf("set %s=0xaddr[,0xaddr…] to pull tokens back into the master", envRecover)
	}
	h := setup(t, needs{})

	abi := usdt.NewUsdt()
	var recovered int
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if !common.IsHexAddress(raw) {
			t.Fatalf("%q is not a hex address", raw)
		}
		from := common.HexToAddress(raw)

		held := h.tokenBalance(from)
		if held.Sign() == 0 {
			t.Logf("%s holds nothing", from.Hex())
			continue
		}
		out, err := h.chain.Call(h.ctx, h.token, abi.PackAllowance(from, h.master))
		if err != nil {
			t.Fatalf("allowance for %s: %v", from.Hex(), err)
		}
		allowed, err := abi.UnpackAllowance(out)
		if err != nil {
			t.Fatalf("decode allowance: %v", err)
		}
		if allowed.Cmp(held) < 0 {
			// Only a wallet that never finished activation lands here, and
			// nothing but its own key can move those funds.
			t.Errorf("%s holds %s but the master may only move %v — it was never activated",
				from.Hex(), fmtToken(held), allowed)
			continue
		}

		before := h.tokenBalance(h.master)
		t.Logf("recovering %s tokens from %s", fmtToken(held), from.Hex())
		h.signAndSend(h.masterKey, h.token, new(big.Int),
			abi.PackTransferFrom(from, h.master, held))
		h.awaitBalance("tokens recovered", func() *big.Int {
			return h.tokenBalance(h.master)
		}, new(big.Int).Add(before, held))
		recovered++
	}
	t.Logf("recovered from %d wallet(s); master now holds %s",
		recovered, fmtToken(h.tokenBalance(h.master)))
}
