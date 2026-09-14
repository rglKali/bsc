package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The wire contract is a payments provider's, not a chain's. An app asks for an
// address, is told what arrived and what it may spend, and asks for a payout.
// How any of that happens on-chain — wei, blocks, log indexes, allowances,
// nonces, gas, the internal flow states — is ours, and an app that never sees it
// cannot come to depend on it (§27).
//
// This test exists because that is a property of every response rather than of
// any one handler, so it is the sort of thing a well-meaning addition puts back
// without noticing. It walks the real surface and fails on the vocabulary
// itself.
func TestNoChainVocabularyOnTheWire(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.deposit("df", "cust-1", 100, 3, 500)
	f.credit("df", 1000)
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: dest, AmountCents: "100",
	}), http.StatusCreated, nil)

	// Field names an app must never be handed. Values are checked too: a wei
	// figure rendered into a differently-named field is the same leak.
	banned := []string{
		"wei", "block", "log_index", "logindex", "drain", "nonce",
		"gas", "allowance", "approve", "funding", "master", "flow",
	}

	for _, path := range []string{
		"/v1/apps/df",
		"/v1/apps/df/balance",
		"/v1/apps/df/addresses",
		"/v1/apps/df/addresses/cust-1",
		"/v1/apps/df/deposits",
		"/v1/apps/df/deposits?status=pending",
		"/v1/apps/df/withdrawals",
		"/v1/apps/df/withdrawals?status=all",
	} {
		w := f.do("GET", path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		body := w.Body.String()
		for _, word := range banned {
			if strings.Contains(strings.ToLower(body), word) {
				t.Errorf("%s leaks %q: %s", path, word, body)
			}
		}

		// Every amount is a decimal string of cents in a *_cents field. A JSON
		// number would invite a float, and a float cannot hold money.
		var any map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &any); err != nil {
			continue // a list envelope, covered by the word check above
		}
		for k, v := range any {
			if strings.HasSuffix(k, "_cents") && !strings.HasPrefix(string(v), `"`) {
				t.Errorf("%s: %s = %s, want a decimal string", path, k, v)
			}
		}
	}
}

// The two lifecycles an app sees are two words each, and both terminal words
// name what happened to its balance rather than what happened on the chain.
func TestAppVisibleStatusesAreTheWholeSet(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.deposit("df", "cust-1", 100, 3, 500)
	f.credit("df", 1000)
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: dest, AmountCents: "100",
	}), http.StatusCreated, nil)

	var deposits depositsPage
	f.json(f.do("GET", "/v1/apps/df/deposits", nil), http.StatusOK, &deposits)
	for _, d := range deposits.Deposits {
		if d.Status != "pending" && d.Status != "credited" {
			t.Errorf("deposit status %q is outside {pending, credited}", d.Status)
		}
	}

	var list struct {
		Withdrawals []withdrawalView `json:"withdrawals"`
	}
	f.json(f.do("GET", "/v1/apps/df/withdrawals?status=all", nil), http.StatusOK, &list)
	for _, wd := range list.Withdrawals {
		if wd.Status != "pending" && wd.Status != "debited" {
			t.Errorf("withdrawal status %q is outside {pending, debited}", wd.Status)
		}
	}
}

// The balance splits without remainder: every cent the app is owed sits in
// exactly one of available, reserved and pending, and total is the three added
// up. An app should never have to work out which pair to add.
func TestBalancePartsSumToTotal(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/addresses", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.credit("df", 1000)
	f.deposit("df", "cust-1", 100, 3, 250) // arrives, not yet drained
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: dest, AmountCents: "100",
	}), http.StatusCreated, nil)

	var b balanceView
	f.json(f.do("GET", "/v1/apps/df/balance", nil), http.StatusOK, &b)
	if b.AvailableCents != "899" || b.ReservedCents != "101" || b.PendingCents != "250" {
		t.Fatalf("balance = %+v, want 899 available / 101 reserved / 250 pending", b)
	}
	if b.TotalCents != "1250" {
		t.Fatalf("total = %s, want 1250", b.TotalCents)
	}
}

// A withdrawal has no failure state, so every permanently-unpayable destination
// has to be refused at creation rather than settled into a terminal status
// afterwards (§28). The zero address is the only one a valid hex address can be
// while still being unpayable — the token reverts on it — and checking costs no
// RPC call.
//
// This matters more than it looks: accepted, it would hold its reservation
// against the app's balance through a retry that could never succeed.
func TestZeroAddressDestinationIsRefusedAtCreation(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: "0x0000000000000000000000000000000000000000", AmountCents: "100",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body)
	}

	// Refused means refused: no record, and nothing reserved against the app.
	var list struct {
		Withdrawals []withdrawalView `json:"withdrawals"`
	}
	f.json(f.do("GET", "/v1/apps/df/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 0 {
		t.Fatalf("a refused request became a record: %+v", list.Withdrawals)
	}
	var b balanceView
	f.json(f.do("GET", "/v1/apps/df/balance", nil), http.StatusOK, &b)
	if b.ReservedCents != "0" || b.AvailableCents != "1000" {
		t.Fatalf("balance = %+v, want nothing held against a refused request", b)
	}
}
