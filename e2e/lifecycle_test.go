//go:build e2e

package e2e

import (
	"context"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bsc/app"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

type walletView struct {
	Ref       string `json:"ref"`
	Address   string `json:"address"`
	DrainTo   string `json:"drain_to"`
	Balance   string `json:"balance"`
	Committed string `json:"committed"`
	Available string `json:"available"`
}

type depositsPage struct {
	Deposits []struct {
		ID      string `json:"id"`
		Wallet  string `json:"wallet"`
		Amount  string `json:"amount"`
		Status  string `json:"status"`
		TxHash  string `json:"tx_hash"`
		SweptBy string `json:"swept_by"`
		SweptTx string `json:"swept_tx"`
	} `json:"deposits"`
	Cursor string `json:"cursor"`
}

type createdView struct {
	Payout withdrawalView  `json:"payout"`
	Fee    *withdrawalView `json:"fee,omitempty"`
}

type withdrawalView struct {
	ID        string `json:"id"`
	Wallet    string `json:"wallet"`
	Reason    string `json:"reason"`
	PartOf    string `json:"part_of"`
	Cursor    string `json:"cursor"`
	Status    string `json:"status"`
	To        string `json:"to"`
	Amount    string `json:"amount"`
	TxHash    string `json:"tx_hash"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

// TestLifecycle walks real money through the whole service on a real chain: a
// wallet that accumulates, a forwarding wallet that drains into it, a deposit
// from an outside address, the automatic forward, and a payout — then audits
// the result against the chain itself.
//
// It runs as one sequence of subtests because every stage shares the master's
// single nonce lane; running them in parallel would violate the invariant the
// sender exists to hold.
func TestLifecycle(t *testing.T) {
	h := setup(t, needs{deposit: true})

	suffix := uuid.New().String()[:8]
	treasury := "e2e-treasury-" + suffix
	user := "e2e-user-" + suffix

	var (
		treasuryAddr common.Address
		userAddr     common.Address
		withdrawal   withdrawalView
	)

	t.Run("deriving a wallet spends nothing", func(t *testing.T) {
		before, err := h.chain.BalanceBNB(h.ctx, h.master)
		if err != nil {
			t.Fatalf("master balance: %v", err)
		}

		var w walletView
		h.call("PUT", "/v1/wallets/"+treasury, map[string]any{}, http.StatusCreated, &w)
		treasuryAddr = common.HexToAddress(w.Address)
		t.Logf("treasury %s → %s", treasury, w.Address)

		if w.DrainTo != "" {
			t.Fatalf("drain_to = %q, want a wallet that keeps what it receives", w.DrainTo)
		}

		// Nothing is signed and no gas is paid: an address book of users who
		// register and never deposit must cost nothing (§41). On a real chain
		// this is the assertion that matters, because activation is two real
		// transactions and the bill is real.
		after, err := h.chain.BalanceBNB(h.ctx, h.master)
		if err != nil {
			t.Fatalf("master balance: %v", err)
		}
		if after.Cmp(before) != 0 {
			t.Fatalf("master gas went from %s to %s deriving a wallet", before, after)
		}
		h.view(func(tx *store.Tx) error {
			got, ok, err := tx.WalletByRef(treasury)
			if err != nil || !ok {
				t.Fatalf("lookup: ok=%v err=%v", ok, err)
			}
			if got.Active {
				t.Fatal("a freshly derived wallet is already active")
			}
			return nil
		})
	})

	t.Run("a forwarding wallet is pointed at it", func(t *testing.T) {
		var w walletView
		h.call("PUT", "/v1/wallets/"+user,
			map[string]any{"drain_to": treasuryAddr.Hex()},
			http.StatusCreated, &w)
		userAddr = common.HexToAddress(w.Address)
		t.Logf("user %s → %s, forwarding to %s", user, w.Address, w.DrainTo)

		if !common.IsHexAddress(w.DrainTo) || common.HexToAddress(w.DrainTo) != treasuryAddr {
			t.Fatalf("drain_to = %q, want the treasury", w.DrainTo)
		}
	})

	t.Run("a cycle is refused by the real service", func(t *testing.T) {
		// The check that exists because §32 made topology settable: pointing the
		// treasury back at the user would loop, and every hop would succeed.
		h.call("PATCH", "/v1/wallets/"+treasury,
			map[string]any{"drain_to": userAddr.Hex()},
			http.StatusUnprocessableEntity, nil)
	})

	t.Run("a deposit is recorded and forwarded on its own", func(t *testing.T) {
		h.sendTokens(userAddr, h.deposit)
		t.Logf("sent %s tokens to %s", fmtToken(h.deposit), userAddr.Hex())

		var page depositsPage
		h.pump("the deposit to be recorded", func() bool {
			h.call("GET", "/v1/wallets/"+user+"/deposits", nil, http.StatusOK, &page)
			return len(page.Deposits) > 0
		})

		d := page.Deposits[0]
		// The amount is the chain's own figure, to the last unit. Nothing is
		// floored, so this is exactly what a block explorer shows (§36).
		if d.Amount != h.deposit.String() {
			t.Fatalf("amount = %s, want %s exactly", d.Amount, h.deposit)
		}
		if d.TxHash == "" {
			t.Fatal("no tx hash on the deposit")
		}

		// The drain is not scheduled anywhere: the work rules noticed a
		// forwarding wallet holding more than the threshold, and converged.
		//
		// This is also where activation happens — funding then approve, as the
		// prefix of the drain rather than a cost paid up front (§41). Gas
		// estimation is the thing a simulator cannot check: if the estimate is
		// short, the approve simply fails on a real chain.
		h.pump("the deposit to be forwarded", func() bool {
			h.call("GET", "/v1/wallets/"+user+"/deposits", nil, http.StatusOK, &page)
			return len(page.Deposits) > 0 && page.Deposits[0].Status == "forwarded"
		})
		if page.Deposits[0].SweptBy == "" {
			t.Fatal("a forwarded deposit was not linked to the debit that carried it")
		}

		h.awaitBalance("the treasury to hold the deposit",
			func() *big.Int { return h.tokenBalance(treasuryAddr) }, h.deposit)
		h.awaitBalance("the forwarding wallet to be empty",
			func() *big.Int { return h.tokenBalance(userAddr) }, new(big.Int))

		// The drain is what activated it, which is the whole point of lazy
		// activation: the wallet paid for its allowance at the moment it first
		// had something to move.
		h.view(func(tx *store.Tx) error {
			got, ok, err := tx.WalletByRef(user)
			if err != nil || !ok {
				t.Fatalf("lookup: ok=%v err=%v", ok, err)
			}
			if !got.Active {
				t.Fatal("the forwarding wallet moved money without being activated")
			}
			return nil
		})
	})

	t.Run("both ends of the movement are on the feed", func(t *testing.T) {
		// The arrival at the treasury is a deposit like any other. The two
		// records pair by hash, which is how a caller counting money avoids
		// counting one transfer twice (§34).
		var feed depositsPage
		h.call("GET", "/v1/deposits", nil, http.StatusOK, &feed)

		var forwarded, landed *struct {
			ID      string `json:"id"`
			Wallet  string `json:"wallet"`
			Amount  string `json:"amount"`
			Status  string `json:"status"`
			TxHash  string `json:"tx_hash"`
			SweptBy string `json:"swept_by"`
			SweptTx string `json:"swept_tx"`
		}
		for i := range feed.Deposits {
			switch feed.Deposits[i].Wallet {
			case user:
				forwarded = &feed.Deposits[i]
			case treasury:
				landed = &feed.Deposits[i]
			}
		}
		if forwarded == nil || landed == nil {
			t.Fatalf("feed does not carry both ends: %+v", feed.Deposits)
		}
		if forwarded.SweptTx != landed.TxHash {
			t.Fatalf("swept_tx %s does not match the landing tx %s",
				forwarded.SweptTx, landed.TxHash)
		}
	})

	t.Run("a payout moves exactly what was asked for", func(t *testing.T) {
		dest := h.destination
		before := h.tokenBalance(dest)

		// Half the deposit, so the treasury is still funded afterwards.
		amount := new(big.Int).Div(h.deposit, big.NewInt(2))

		var created createdView
		h.call("POST", "/v1/wallets/"+treasury+"/withdrawals", map[string]any{
			"to": dest.Hex(), "amount": amount.String(),
			"idempotency_key": "e2e-" + suffix,
		}, http.StatusCreated, &created)
		withdrawal = created.Payout
		if withdrawal.Status != "pending" || withdrawal.Reason != "payout" {
			t.Fatalf("payout = %+v", withdrawal)
		}

		// While it is pending the treasury has promised it.
		var w walletView
		h.call("GET", "/v1/wallets/"+treasury, nil, http.StatusOK, &w)
		if w.Committed != amount.String() {
			t.Fatalf("committed = %s, want %s", w.Committed, amount)
		}

		// A retry replays rather than paying twice — on a real chain this is the
		// failure that is unrecoverable.
		var again createdView
		h.call("POST", "/v1/wallets/"+treasury+"/withdrawals", map[string]any{
			"to": dest.Hex(), "amount": amount.String(),
			"idempotency_key": "e2e-" + suffix,
		}, http.StatusCreated, &again)
		if again.Payout.ID != withdrawal.ID {
			t.Fatalf("idempotency replayed as a new withdrawal: %s vs %s",
				again.Payout.ID, withdrawal.ID)
		}

		h.pump("the payout to confirm", func() bool {
			var got withdrawalView
			h.call("GET", "/v1/withdrawals/"+withdrawal.ID, nil, http.StatusOK, &got)
			if got.Attempts > 0 && got.LastError != "" {
				t.Logf("attempt %d: %s", got.Attempts, got.LastError)
			}
			withdrawal = got
			return got.Status == "confirmed"
		})

		// Exactly the amount, and nothing withheld: bsc charges no fee (§33).
		want := new(big.Int).Add(before, amount)
		h.awaitBalance("the destination to receive exactly the amount",
			func() *big.Int { return h.tokenBalance(dest) }, want)

		h.call("GET", "/v1/wallets/"+treasury, nil, http.StatusOK, &w)
		if w.Committed != "0" {
			t.Fatalf("committed = %s after settlement, want 0", w.Committed)
		}
		remaining := new(big.Int).Sub(h.deposit, amount)
		if w.Balance != remaining.String() {
			t.Fatalf("treasury balance = %s, want %s — nothing is kept back", w.Balance, remaining)
		}
	})

	t.Run("the settled feed carries the drain and the payout", func(t *testing.T) {
		// One feed of everything that left, whoever decided it — the property
		// that made a drain stop being a third kind of thing (§42).
		var feed struct {
			Withdrawals []withdrawalView `json:"withdrawals"`
			Cursor      string           `json:"cursor"`
		}
		h.call("GET", "/v1/withdrawals", nil, http.StatusOK, &feed)

		seen := map[string]bool{}
		for _, wd := range feed.Withdrawals {
			seen[wd.Reason] = true
			if wd.Status != "confirmed" {
				t.Errorf("the settled feed carries a %s debit: %+v", wd.Status, wd)
			}
			if wd.Cursor == "" {
				t.Errorf("a settled debit has no cursor: %+v", wd)
			}
		}
		if !seen["drain"] || !seen["payout"] {
			t.Fatalf("feed = %+v, want both the forward and the payout", feed.Withdrawals)
		}

		// Passing the cursor back yields nothing new.
		var again struct {
			Withdrawals []withdrawalView `json:"withdrawals"`
		}
		h.call("GET", "/v1/withdrawals?since="+feed.Cursor, nil, http.StatusOK, &again)
		if len(again.Withdrawals) != 0 {
			t.Fatalf("resuming past the end returned %d debits", len(again.Withdrawals))
		}
	})

	t.Run("our record of custody agrees with the token", func(t *testing.T) {
		// The check a simulator cannot make, because a simulator *is* our view.
		rep, err := h.audit()
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		for _, f := range rep.Findings {
			t.Errorf("audit finding: %s", f)
		}

		path := filepath.Join(t.TempDir(), "snapshot.db")
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create snapshot: %v", err)
		}
		if _, err := h.store.Snapshot(f); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		f.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		withChain, err := app.VerifyOnChain(ctx, path, h.rpcURL, 20, h.token)
		if err != nil {
			t.Fatalf("verify --rpc: %v", err)
		}
		for _, finding := range withChain.Findings {
			t.Errorf("chain audit finding: %s", finding)
		}
	})
}

// A payout larger than the wallet holds is refused before anything is signed,
// so it costs no gas and leaves no record to clean up.
func TestRejectsOverdraft(t *testing.T) {
	h := setup(t, needs{})

	ref := "e2e-empty-" + uuid.New().String()[:8]
	var w walletView
	h.call("PUT", "/v1/wallets/"+ref, map[string]any{}, http.StatusCreated, &w)

	h.call("POST", "/v1/wallets/"+ref+"/withdrawals", map[string]any{
		"to": h.destination.Hex(), "amount": whole(1_000_000).String(),
	}, http.StatusUnprocessableEntity, nil)

	var list struct {
		Withdrawals []withdrawalView `json:"withdrawals"`
	}
	h.call("GET", "/v1/wallets/"+ref+"/withdrawals?status=all", nil, http.StatusOK, &list)
	for _, got := range list.Withdrawals {
		if got.Wallet == ref {
			t.Fatalf("a refused request became a record: %+v", got)
		}
	}
}
