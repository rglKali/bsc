//go:build e2e

package e2e

import (
	"context"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"bsc/app"
	"bsc/money"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

type appView struct {
	Slug    string `json:"slug"`
	Address string `json:"address"`
	Balance struct {
		AvailableCents string `json:"available_cents"`
		ReservedCents  string `json:"reserved_cents"`
		PendingCents   string `json:"pending_cents"`
	} `json:"balance"`
}

type addressView struct {
	Ref     string `json:"ref"`
	Address string `json:"address"`
}

type depositsPage struct {
	Deposits []struct {
		ID          string `json:"id"`
		Ref         string `json:"ref"`
		AmountCents string `json:"amount_cents"`
		Status      string `json:"status"`
		TxHash      string `json:"tx_hash"`
	} `json:"deposits"`
	Cursor string `json:"cursor"`
}

type withdrawalView struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	FeeCents    string `json:"fee_cents"`
	PayoutCents string `json:"payout_cents"`
	TxHash      string `json:"tx_hash"`
	Attempts    int    `json:"attempts"`
	LastError   string `json:"last_error"`
}

// TestLifecycle walks real money through the whole service on a real chain:
// registration and activation, a deposit from an outside wallet, the automatic
// drain, a payout, and the house sweep that collects the fee — then audits the
// result against the chain itself.
//
// It runs as one sequence of subtests because every stage shares the master's
// single nonce lane; running them in parallel would violate the invariant the
// sender exists to hold.
func TestLifecycle(t *testing.T) {
	h := setup(t, needs{deposit: true})
	slug := "e2e-" + uuid.New().String()[:8]
	dest := h.destination

	var (
		appInfo    appView
		depositAdr addressView
		withdrawal withdrawalView
		// Captured before the payout is requested: see the note there.
		collectorBefore *big.Int
	)

	t.Run("register", func(t *testing.T) {
		// Registration derives the hot wallet and pre-warms it, so the first
		// payout does not also pay for activation.
		h.call("PUT", "/v1/apps/"+slug, map[string]any{}, http.StatusCreated, &appInfo)
		t.Logf("app %s → hot wallet %s", slug, appInfo.Address)

		hot := common.HexToAddress(appInfo.Address)
		h.pump("hot wallet activated on-chain", func() bool {
			return h.active(hot)
		})
	})

	t.Run("deposit address", func(t *testing.T) {
		h.call("POST", "/v1/apps/"+slug+"/addresses",
			map[string]any{"ref": "cust-1"}, http.StatusCreated, &depositAdr)
		if !common.IsHexAddress(depositAdr.Address) {
			t.Fatalf("bad address %q", depositAdr.Address)
		}
		t.Logf("deposit address for cust-1 → %s", depositAdr.Address)
	})

	t.Run("deposit is detected and drained", func(t *testing.T) {
		target := common.HexToAddress(depositAdr.Address)
		h.sendTokens(target, h.deposit)

		// Detection: the watcher sees the transfer in a finalized block.
		var page depositsPage
		h.pump("deposit detected", func() bool {
			h.call("GET", "/v1/apps/"+slug+"/deposits", nil, http.StatusOK, &page)
			return len(page.Deposits) > 0
		})
		got := page.Deposits[0]
		depositCents, _ := h.scale.ToCents(h.deposit)
		if got.Ref != "cust-1" || got.AmountCents != depositCents.String() {
			t.Fatalf("deposit = %+v, want %s cents to cust-1", got, depositCents)
		}
		// The app is shown cents and a hash it can look up, and nothing about
		// how the money moved — no wei, no block, no log index (§27).
		if got.ID == "" || got.TxHash == "" {
			t.Fatalf("deposit = %+v, want an id and a transaction hash", got)
		}

		// The drain is nobody's request: the rules noticed the balance and
		// started it. A fresh deposit wallet is activated inside that flow —
		// funded with just enough gas, then approving the master — so watch
		// that happen before the sweep itself.
		h.pump("deposit wallet activated on-chain", func() bool {
			return h.active(target)
		})
		if gas, err := h.chain.BalanceBNB(h.ctx, target); err != nil {
			t.Fatalf("deposit wallet gas: %v", err)
		} else if gas.Sign() == 0 {
			t.Fatal("deposit wallet was activated without ever being funded")
		}

		// Wait on the *record*, not the chain. A sweep is visible on-chain the
		// moment it mines, but the service only credits at finality — so
		// polling the balance would race ahead of the service and see a state
		// it has not reached yet. "Credited" is the stronger condition and
		// implies the weaker one.
		hot := common.HexToAddress(appInfo.Address)
		h.pump("deposit credited", func() bool {
			h.call("GET", "/v1/apps/"+slug+"/deposits", nil, http.StatusOK, &page)
			return len(page.Deposits) > 0 && page.Deposits[0].Status == "credited"
		})
		if got := h.tokenBalance(hot); got.Cmp(h.deposit) < 0 {
			t.Fatalf("hot wallet holds %s on-chain, want the whole deposit %s",
				fmtToken(got), fmtToken(h.deposit))
		}
		if left := h.tokenBalance(target); left.Sign() != 0 {
			t.Fatalf("deposit wallet still holds %s", fmtToken(left))
		}

		h.call("GET", "/v1/apps/"+slug, nil, http.StatusOK, &appInfo)
		if appInfo.Balance.AvailableCents != depositCents.String() {
			t.Fatalf("available = %s cents, want the whole deposit %s",
				appInfo.Balance.AvailableCents, depositCents)
		}
		t.Logf("available %s cents · pending %s", appInfo.Balance.AvailableCents,
			appInfo.Balance.PendingCents)
	})

	t.Run("withdrawal pays out", func(t *testing.T) {
		const payoutCents = 100 // $1.00
		payout := h.scale.Wei(payoutCents)
		before := h.tokenBalance(dest)
		// Read the collector *before the withdrawal exists*. The house sweep is
		// nobody's request — the rules start it as soon as the wallet is free —
		// so it can land while we are still pumping this subtest. A baseline
		// taken afterwards could already include it, and the next assertion
		// would then wait forever for a rise that already happened.
		collectorBefore = h.tokenBalance(h.collector)

		h.call("POST", "/v1/apps/"+slug+"/withdrawals", map[string]any{
			"destination": dest.Hex(), "amount_cents": "100",
			"idempotency_key": "e2e-1",
		}, http.StatusCreated, &withdrawal)
		if withdrawal.Status != "pending" {
			t.Fatalf("status = %s", withdrawal.Status)
		}

		// A retry must replay rather than pay twice — the property that
		// matters most on a real chain, because the second payment would be
		// unrecoverable.
		var replay withdrawalView
		h.call("POST", "/v1/apps/"+slug+"/withdrawals", map[string]any{
			"destination": dest.Hex(), "amount_cents": "100",
			"idempotency_key": "e2e-1",
		}, http.StatusOK, &replay)
		if replay.ID != withdrawal.ID {
			t.Fatalf("retry created a second withdrawal: %s vs %s", replay.ID, withdrawal.ID)
		}

		h.pump("withdrawal settled", func() bool {
			var got withdrawalView
			h.call("GET", "/v1/apps/"+slug+"/withdrawals/"+withdrawal.ID, nil, http.StatusOK, &got)
			withdrawal = got
			// There is no failure state to wait for: a payout that reverts is
			// retried, so the only terminal status is `debited` (§28).
			return got.Status == "debited"
		})
		if withdrawal.Status != "debited" {
			t.Fatalf("withdrawal %s after %d attempts: %s",
				withdrawal.Status, withdrawal.Attempts, withdrawal.LastError)
		}
		if withdrawal.TxHash == "" {
			t.Fatal("settled withdrawal has no transaction hash")
		}
		t.Logf("payout tx %s", withdrawal.TxHash)

		// The destination actually received it, on-chain.
		want := new(big.Int).Add(before, payout)
		h.pump("destination credited on-chain", func() bool {
			return h.tokenBalance(dest).Cmp(want) >= 0
		})
	})

	t.Run("the fee stays behind and the house sweeps it", func(t *testing.T) {
		// The fee never rode along with the payout. It was charged in the
		// ledger and simply left in the wallet, which is what makes a
		// withdrawal one transfer instead of two (§24).
		h.call("GET", "/v1/apps/"+slug, nil, http.StatusOK, &appInfo)
		if appInfo.Balance.ReservedCents != "0" {
			t.Fatalf("reserved = %s after the payout settled, want nothing held",
				appInfo.Balance.ReservedCents)
		}
		if withdrawal.FeeCents == "0" {
			t.Fatal("no fee charged")
		}
		t.Logf("charged %s cents in fees, left in the hot wallet", withdrawal.FeeCents)

		// The collector is the master by default, so this is also the check that
		// the master accumulates fee income — the balance a USDT→BNB gas top-up
		// draws on. The bound is monotone (at least the baseline plus the fee),
		// so it holds whether the sweep landed before this line or after it.
		fee, err := strconv.ParseInt(withdrawal.FeeCents, 10, 64)
		if err != nil {
			t.Fatalf("fee %q: %v", withdrawal.FeeCents, err)
		}
		want := new(big.Int).Add(collectorBefore, h.scale.Wei(money.Cents(fee)))
		h.pump("house excess swept to the collector", func() bool {
			return h.tokenBalance(h.collector).Cmp(want) >= 0
		})
		t.Logf("collector %s now holds %s", h.collector.Hex(), fmtToken(h.tokenBalance(h.collector)))

		// And what is left in the hot wallet is exactly what the app is owed:
		// the sweep took the house's share and nothing else (§25).
		//
		// Wait on our *record* of custody, not on the chain. The sweep is
		// visible on-chain the moment it mines, but the service only applies it
		// at finality — so a chain-only wait would let the next subtest audit a
		// record that has not caught up yet and report a balance divergence
		// that is really just a transaction in flight.
		hot := common.HexToAddress(appInfo.Address)
		owed, err := strconv.ParseInt(appInfo.Balance.AvailableCents, 10, 64)
		if err != nil {
			t.Fatalf("available %q: %v", appInfo.Balance.AvailableCents, err)
		}
		h.pump("the sweep reaches our records", func() bool {
			return h.scale.Excess(h.custody(hot), money.Cents(owed)).Sign() == 0
		})
		if excess := h.scale.Excess(h.tokenBalance(hot), money.Cents(owed)); excess.Sign() != 0 {
			t.Fatalf("hot wallet holds %s over the ledger on-chain, want nothing", excess)
		}
	})

	t.Run("audit against the chain", func(t *testing.T) {
		// The offline audit recomputes every ledger from the log and checks
		// each app is solvent against our record of custody; --rpc is the only
		// thing that can catch that record diverging from the token's own view,
		// which is precisely what a real chain can reveal and a simulator
		// cannot.
		rep, err := h.audit()
		if err != nil {
			t.Fatalf("offline audit: %v", err)
		}
		if !rep.OK() {
			t.Fatalf("offline audit findings: %v", rep.Findings)
		}
		path := h.snapshot(t)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		rep, err = app.VerifyOnChain(ctx, path, h.rpcURL, 20, h.token)
		if err != nil {
			t.Fatalf("on-chain audit: %v", err)
		}
		if !rep.OK() {
			t.Fatalf("on-chain audit findings — our balances disagree with the token: %v", rep.Findings)
		}
		t.Logf("audit clean · %d wallets, %d deposits, %d withdrawals",
			rep.Wallets, rep.Deposits, rep.Withdrawals)
	})
}

// TestRejectsOverdraft proves the balance guard on a real chain: the refusal
// must happen before anything is signed, not after a transfer reverts.
func TestRejectsOverdraft(t *testing.T) {
	h := setup(t, needs{})
	slug := "e2e-od-" + uuid.New().String()[:8]

	var view appView
	h.call("PUT", "/v1/apps/"+slug, map[string]any{}, http.StatusCreated, &view)

	// A fresh app has nothing, so any payout must be refused outright.
	h.call("POST", "/v1/apps/"+slug+"/withdrawals", map[string]any{
		"destination":  h.destination.Hex(),
		"amount_cents": "100",
	}, http.StatusUnprocessableEntity, nil)

	var after appView
	h.call("GET", "/v1/apps/"+slug, nil, http.StatusOK, &after)
	if after.Balance.ReservedCents != "0" {
		t.Fatalf("a refused withdrawal left state behind: %+v", after)
	}
}

// active reports whether a wallet has approved the master, read from the chain
// rather than from our own record.
func (h *harness) active(wallet common.Address) bool {
	h.t.Helper()
	var known bool
	if err := h.store.View(func(tx *store.Tx) error {
		w, ok, err := tx.WalletByAddress(wallet)
		if err != nil {
			return err
		}
		known = ok && w.Active
		return nil
	}); err != nil {
		h.t.Fatalf("View: %v", err)
	}
	return known
}

// snapshot writes the live database to a file the audit can open read-only.
func (h *harness) snapshot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	defer f.Close()
	if _, err := h.store.Snapshot(f); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return path
}
