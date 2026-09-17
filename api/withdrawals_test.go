package api

import (
	"net/http"
	"testing"
)

const dest = "0x00000000000000000000000000000000000000dd"

type withdrawalsPage struct {
	Withdrawals []withdrawalView `json:"withdrawals"`
	Cursor      string           `json:"cursor"`
}

func TestCreateWithdrawalCommitsAgainstCustody(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	var got createdView
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "400",
	}), http.StatusCreated, &got)

	wd := got.Payout
	if wd.Status != "pending" || wd.Amount != "400" || wd.Wallet != "hot" || wd.Reason != "payout" {
		t.Fatalf("payout = %+v", wd)
	}
	if got.Fee != nil {
		t.Fatalf("a fee appeared without being asked for: %+v", got.Fee)
	}
	var w walletView
	f.json(f.do("GET", "/v1/wallets/hot", nil), http.StatusOK, &w)
	if w.Committed != "400" || w.Available != "600" {
		t.Fatalf("wallet = %+v, want 400 committed and 600 available", w)
	}
}

// The overdraft guard is the whole of what replaced the reservation ledger:
// what is already promised, plus what is being asked for, against what the
// chain says the wallet holds (§39).
func TestWithdrawalRefusesToPromiseMoreThanIsHeld(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "700",
	}), http.StatusCreated, nil)

	// 700 is already committed, so 400 more overdraws even though the balance
	// alone would cover it.
	w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{To: dest, Amount: "400"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}

	// And the refusal left nothing behind.
	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 {
		t.Fatalf("withdrawals = %d, want only the accepted one", len(list.Withdrawals))
	}
}

// Forwarding and paying out contradict each other — one empties the wallet, the
// other spends from it — so a proxy wallet refuses payouts outright (§32).
func TestWithdrawalRefusedFromAForwardingWallet(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.proxy("cust-1", hot.Address)
	f.credit("cust-1", 1_000)

	w := f.do("POST", "/v1/wallets/cust-1/withdrawals", withdrawalBody{To: dest, Amount: "100"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
}

func TestWithdrawalRefusedFromAPausedWallet(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)
	yes := true
	f.json(f.do("PATCH", "/v1/wallets/hot", patchWalletBody{Paused: &yes}), http.StatusOK, nil)

	w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{To: dest, Amount: "100"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

// A retry after a timeout replays the original instead of paying twice.
func TestIdempotencyKeyReplaysTheOriginal(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	body := withdrawalBody{To: dest, Amount: "100", IdempotencyKey: "order-4417"}
	var first, second createdView
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", body), http.StatusCreated, &first)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", body), http.StatusCreated, &second)
	if first.Payout.ID != second.Payout.ID {
		t.Fatalf("replay created a second withdrawal: %s vs %s", first.Payout.ID, second.Payout.ID)
	}

	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 {
		t.Fatalf("withdrawals = %d, want 1", len(list.Withdrawals))
	}
}

func TestIdempotencyKeyWithDifferentParametersConflicts(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "100", IdempotencyKey: "k",
	}), http.StatusCreated, nil)

	w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "200", IdempotencyKey: "k",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// Balances are only current at the head, so accepting a payout against a stale
// one could overdraw the wallet. Reads keep working throughout (§21).
func TestWithdrawalsAreRefusedWhileSyncing(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)
	f.sync.behind = 10_000

	w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{To: dest, Amount: "100"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if r := f.do("GET", "/v1/wallets/hot", nil); r.Code != http.StatusOK {
		t.Fatalf("reads broke while syncing: %d", r.Code)
	}
}

func TestWithdrawalValidatesItsInputs(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	cases := map[string]withdrawalBody{
		"bad destination": {To: "not-an-address", Amount: "100"},
		"zero amount":     {To: dest, Amount: "0"},
		"negative amount": {To: dest, Amount: "-5"},
		"decimal amount":  {To: dest, Amount: "1.5"},
		"empty amount":    {To: dest, Amount: ""},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if w := f.do("POST", "/v1/wallets/hot/withdrawals", body); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
		})
	}
}

// The caller minted the ids, so it polls its own outstanding set and drops each
// entry as it settles. Pending is the default because that is the set it polls.
func TestWithdrawalListingDefaultsToPending(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)
	var created createdView
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "100",
	}), http.StatusCreated, &created)
	wd := created.Payout

	// A wallet's own listing defaults to what it still owes — the settled ones
	// are on the feed, so what is worth asking a wallet is what is outstanding.
	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 {
		t.Fatalf("pending = %d, want 1", len(list.Withdrawals))
	}
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=confirmed", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 0 {
		t.Fatalf("confirmed = %d, want 0", len(list.Withdrawals))
	}
	if w := f.do("GET", "/v1/wallets/hot/withdrawals?status=nonsense", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d, want 400", w.Code)
	}

	// The settled feed holds only what has happened, so a pending payout is
	// absent from it by construction (§42).
	var feed withdrawalsPage
	f.json(f.do("GET", "/v1/withdrawals", nil), http.StatusOK, &feed)
	if len(feed.Withdrawals) != 0 {
		t.Fatalf("the settled feed carries a pending payout: %+v", feed.Withdrawals)
	}

	var one withdrawalView
	f.json(f.do("GET", "/v1/withdrawals/"+wd.ID, nil), http.StatusOK, &one)
	if one.ID != wd.ID {
		t.Fatalf("lookup = %s, want %s", one.ID, wd.ID)
	}
	if w := f.do("GET", "/v1/withdrawals/not-a-uuid", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d, want 400", w.Code)
	}
}

// Creating work must nudge the sender rather than leave it to a tick.
func TestCreatingAWithdrawalNotifiesTheSender(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)
	before := f.notify

	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "100",
	}), http.StatusCreated, nil)
	if f.notify <= before {
		t.Fatal("the sender was not nudged")
	}
}

// A fee is a second debit, asked for in the same call. bsc does not decide what
// it is — the caller has already done that and is telling us a number — and all
// bsc adds is that the two are accepted together and that it knows where to send
// it (§43).
func TestFeeBecomesASecondDebit(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	var got createdView
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "400", Fee: "25",
	}), http.StatusCreated, &got)

	if got.Fee == nil {
		t.Fatal("no fee record returned")
	}
	if got.Payout.Reason != "payout" || got.Fee.Reason != "fee" {
		t.Fatalf("reasons = %q / %q", got.Payout.Reason, got.Fee.Reason)
	}
	if got.Fee.Amount != "25" || got.Fee.PartOf != got.Payout.ID {
		t.Fatalf("fee = %+v, want 25 linked to the payout", got.Fee)
	}
	// It goes to the wallet that pays for gas, which is not a policy decision
	// and so is not configurable.
	master, _ := f.srv.ring.Master()
	if got.Fee.To != master.Address.Hex() {
		t.Fatalf("fee destination = %s, want the master %s", got.Fee.To, master.Address.Hex())
	}

	// Both are committed, so the wallet cannot spend what it has promised away.
	var w walletView
	f.json(f.do("GET", "/v1/wallets/hot", nil), http.StatusOK, &w)
	if w.Committed != "425" || w.Available != "575" {
		t.Fatalf("wallet = %+v, want 425 committed", w)
	}
}

// Joint acceptance is the whole of what bsc adds over two separate calls: a
// wallet can never end up having accepted the payout and refused the fee (§43).
func TestPayoutAndFeeAreAcceptedTogetherOrNotAtAll(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 100)

	// The payout alone would fit; with the fee it does not.
	w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "90", Fee: "25",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}

	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 0 {
		t.Fatalf("a refused pair left records behind: %+v", list.Withdrawals)
	}
}

// One request, one key: a retry replays both halves.
func TestIdempotentReplayReturnsTheFeeToo(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	body := withdrawalBody{To: dest, Amount: "100", Fee: "10", IdempotencyKey: "k"}
	var first, second createdView
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", body), http.StatusCreated, &first)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", body), http.StatusCreated, &second)

	if second.Fee == nil || first.Fee.ID != second.Fee.ID {
		t.Fatalf("replay did not return the same fee: %+v vs %+v", first.Fee, second.Fee)
	}
	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 2 {
		t.Fatalf("withdrawals = %d, want the payout and its fee once each", len(list.Withdrawals))
	}
}

func TestFeeIsValidatedLikeAnyAmount(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)

	for name, fee := range map[string]string{
		"zero":    "0",
		"decimal": "1.5",
		"words":   "ten",
	} {
		t.Run(name, func(t *testing.T) {
			w := f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
				To: dest, Amount: "100", Fee: fee,
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("fee %q: status %d, want 400", fee, w.Code)
			}
		})
	}
}

// The two settle independently — two transfers cannot be made atomic without a
// contract — so the reason filter is how a caller separates them afterwards.
func TestDebitsAreFilterableByReason(t *testing.T) {
	f := newFixture(t)
	f.wallet("hot")
	f.credit("hot", 1_000)
	f.json(f.do("POST", "/v1/wallets/hot/withdrawals", withdrawalBody{
		To: dest, Amount: "100", Fee: "10",
	}), http.StatusCreated, nil)

	var list withdrawalsPage
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all&reason=fee", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 || list.Withdrawals[0].Reason != "fee" {
		t.Fatalf("fee filter = %+v", list.Withdrawals)
	}
	f.json(f.do("GET", "/v1/wallets/hot/withdrawals?status=all&reason=payout", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 || list.Withdrawals[0].Reason != "payout" {
		t.Fatalf("payout filter = %+v", list.Withdrawals)
	}
	if w := f.do("GET", "/v1/withdrawals?reason=nonsense", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad reason: %d, want 400", w.Code)
	}
}
