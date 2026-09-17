package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The wire contract is a wallet's, not a pipeline's. A caller asks for a
// wallet, is told what arrived on it, and asks for a payout from it.
//
// What is still deliberately absent is the machinery: nonces, gas, allowances,
// the master address, and the internal flow states. Those are ours, and a
// caller that never sees them cannot come to depend on them. What *is* present
// now, and was not before, is the chain's own unit — because with no ledger
// there is no second unit to confuse it with (§36).
//
// This test exists because that is a property of every response rather than of
// any one handler, so it is the sort of thing a well-meaning addition puts back
// without noticing. It walks the real surface and fails on the vocabulary.
func TestNoMachineryVocabularyOnTheWire(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.proxy("cust-1", hot.Address)
	f.deposit("cust-1", 100, 3, 500)
	f.credit("hot", 1000)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: addrHex(0xDD), Amount: "100",
	}), http.StatusCreated, nil)

	// Words a caller must never be handed. Values are checked too: a nonce
	// rendered into a differently-named field is the same leak.
	banned := []string{
		"nonce", "gas", "allowance", "approve", "funding", "master",
		"flow", "sweeping", "prewarm", "ledger", "cents", "fee",
	}

	for _, path := range []string{
		"/v1/wallets",
		"/v1/wallets/hot",
		"/v1/wallets/cust-1",
		"/v1/deposits",
		"/v1/wallets/cust-1/deposits",
		"/v1/withdrawals",
		"/v1/wallets/hot/withdrawals?status=all",
	} {
		w := f.do("GET", path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d — %s", path, w.Code, w.Body.String())
		}
		body := strings.ToLower(w.Body.String())
		for _, word := range banned {
			if strings.Contains(body, word) {
				t.Errorf("%s leaks %q: %s", path, word, w.Body.String())
			}
		}
	}
}

// Every amount is a decimal string, never a JSON number. A uint256 does not
// survive a float64, and money must not be rounded by a parser.
func TestAmountsAreDecimalStrings(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 12_345)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: addrHex(0xDD), Amount: "100",
	}), http.StatusCreated, nil)

	w := f.do("GET", "/v1/wallets/hot", nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"balance", "committed", "available"} {
		v, ok := fields[k]
		if !ok {
			t.Fatalf("%s missing from %s", k, w.Body.String())
		}
		if !strings.HasPrefix(string(v), `"`) {
			t.Errorf("%s = %s, want a decimal string", k, v)
		}
	}
}

// Two lifecycles, two words each, and both name where the money physically is —
// the only thing bsc can honestly report with no ledger behind it (§34).
func TestStatusVocabularyIsTheWholeSet(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.proxy("cust-1", hot.Address)
	f.deposit("cust-1", 100, 3, 500)
	f.credit("hot", 1000)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: addrHex(0xDD), Amount: "100",
	}), http.StatusCreated, nil)

	var deposits depositsPage
	f.json(f.do("GET", "/v1/deposits", nil), http.StatusOK, &deposits)
	if len(deposits.Deposits) == 0 {
		t.Fatal("no deposits to check")
	}
	for _, d := range deposits.Deposits {
		if d.Status != "received" && d.Status != "forwarded" {
			t.Errorf("deposit status %q is outside {received, forwarded}", d.Status)
		}
	}

	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) == 0 {
		t.Fatal("no withdrawals to check")
	}
	for _, wd := range list.Withdrawals {
		if wd.Status != "pending" && wd.Status != "confirmed" {
			t.Errorf("withdrawal status %q is outside {pending, confirmed}", wd.Status)
		}
		if wd.Reason != "payout" && wd.Reason != "fee" && wd.Reason != "drain" {
			t.Errorf("debit reason %q is outside {payout, fee, drain}", wd.Reason)
		}
	}
}

// A withdrawal has no failure state, so every permanently-unpayable destination
// has to be refused at creation rather than settled into a terminal status
// afterwards (§28). The zero address is the only one a valid hex address can be
// while still being unpayable — the token reverts on it — and checking costs no
// RPC call.
//
// Accepted, it would hold its commitment against the wallet through a retry
// that could never succeed.
func TestZeroAddressDestinationIsRefusedAtCreation(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1000)

	w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: "0x0000000000000000000000000000000000000000", Amount: "100",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body)
	}

	// Refused means refused: no record, and nothing committed against the wallet.
	var list withdrawalsPage
	f.json(f.do("GET", "/v1/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 0 {
		t.Fatalf("a refused request became a record: %+v", list.Withdrawals)
	}
	var got walletView
	f.json(f.do("GET", "/v1/wallets/hot", nil), http.StatusOK, &got)
	if got.Committed != "0" || got.Available != "1000" {
		t.Fatalf("wallet = %+v, want nothing held against a refused request", got)
	}
}
