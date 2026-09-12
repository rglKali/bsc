package api

import (
	"net/http"
	"testing"

	"bsc/store"

	"github.com/google/uuid"
)

const dest = "0x00000000000000000000000000000000000000dd"

func TestQuoteHasNoSideEffects(t *testing.T) {
	f := newFixture(t)
	f.register("df") // default fee: 1 wei flat

	var q quoteView
	f.json(f.do("POST", "/v1/apps/df/withdrawals/quote", quoteBody{AmountCents: "10"}), http.StatusOK, &q)
	if q.PayoutCents != "10" || q.DebitCents != "11" || q.FeeCents != "1" {
		t.Fatalf("quote = %+v, want the fee charged on top", q)
	}

	f.json(f.do("POST", "/v1/apps/df/withdrawals/quote", quoteBody{AmountCents: "10", DeductFee: true}), http.StatusOK, &q)
	if q.PayoutCents != "9" || q.DebitCents != "10" {
		t.Fatalf("quote = %+v, want the fee deducted", q)
	}

	// Nothing was reserved and nothing queued.
	var app appView
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, &app)
	if app.Balance.ReservedCents != "0" {
		t.Fatalf("quoting reserved %s", app.Balance.ReservedCents)
	}
}

func TestQuoteEnforcesTheAppsMinimum(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.json(f.do("PUT", "/v1/apps/df", appBody{Fee: &feeBody{FlatCents: "1", MinCents: "100"}}), http.StatusOK, nil)

	w := f.do("POST", "/v1/apps/df/withdrawals/quote", quoteBody{AmountCents: "99"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
}

func TestCreateWithdrawalReservesPayoutAndFeeSeparately(t *testing.T) {
	// One reservation covers payout and fee. They leave the ledger together now
	// that the fee never moves on its own, so the figure is the whole
	// commitment and there is no second lifetime to track (§24).
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	var wd withdrawalView
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: dest, AmountCents: "100",
	}), http.StatusCreated, &wd)

	if wd.Status != "queued" || wd.PayoutCents != "100" || wd.FeeCents != "1" {
		t.Fatalf("withdrawal = %+v", wd)
	}

	var app appView
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, &app)
	if app.Balance.ReservedCents != "101" {
		t.Fatalf("reserved = %s, want payout+fee", app.Balance.ReservedCents)
	}
	if app.Balance.AvailableCents != "899" {
		t.Fatalf("available = %s, want 899", app.Balance.AvailableCents)
	}
	if f.notify == 0 {
		t.Fatal("the sender was not nudged")
	}
}

func TestWithdrawalRefusedWhenTheBalanceCannotCoverIt(t *testing.T) {
	// v1 discovered this only when the transfer reverted on-chain, after the
	// gas was spent.
	f := newFixture(t)
	f.register("df")
	f.credit("df", 50)

	w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}

	// The refused request must leave nothing behind.
	var app appView
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, &app)
	if app.Balance.ReservedCents != "0" {
		t.Fatalf("a refused withdrawal left state behind: %+v", app.Balance)
	}
	var list struct {
		Withdrawals []withdrawalView `json:"withdrawals"`
	}
	f.json(f.do("GET", "/v1/apps/df/withdrawals", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 0 {
		t.Fatalf("a refused withdrawal was recorded: %+v", list.Withdrawals)
	}
}

func TestFeeMustAlsoFitTheBalance(t *testing.T) {
	// Exactly enough for the payout but not the fee must still be refused.
	f := newFixture(t)
	f.register("df")
	f.credit("df", 100)

	if w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"}); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	// Deducting the fee instead makes it fit.
	if w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100", DeductFee: true}); w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", w.Code, w.Body.String())
	}
}

func TestIdempotencyKeyReplaysRatherThanPayingTwice(t *testing.T) {
	// Over HTTP a client retry after a timeout would otherwise double-pay; v1
	// got this free from the broker's message-id dedup.
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	body := withdrawalBody{Destination: dest, AmountCents: "100", IdempotencyKey: "k-1"}
	var first, second withdrawalView
	f.json(f.do("POST", "/v1/apps/df/withdrawals", body), http.StatusCreated, &first)
	f.json(f.do("POST", "/v1/apps/df/withdrawals", body), http.StatusOK, &second)

	if first.ID != second.ID {
		t.Fatalf("retry created a second withdrawal: %s vs %s", first.ID, second.ID)
	}
	var app appView
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, &app)
	if app.Balance.ReservedCents != "101" {
		t.Fatalf("reserved = %s, want a single reservation", app.Balance.ReservedCents)
	}
}

func TestIdempotencyKeyReusedWithDifferentParametersConflicts(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: dest, AmountCents: "100", IdempotencyKey: "k-1",
	}), http.StatusCreated, nil)

	w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{
		Destination: dest, AmountCents: "200", IdempotencyKey: "k-1",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
}

func TestWithdrawalsAreRefusedWhileTheChainIsStale(t *testing.T) {
	// Balances are only current at the head. Reserving against a stale balance
	// could overdraw an app, so the honest answer while catching up is "not yet".
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)
	f.sync.behind = 5000

	w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}

	// Reads keep working while syncing; only spending is held back.
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, nil)
	f.json(f.do("GET", "/v1/apps/df/deposits", nil), http.StatusOK, nil)

	f.sync.behind = 0
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"}),
		http.StatusCreated, nil)
}

func TestPausedAppCannotPayOut(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)
	paused := true
	f.json(f.do("PUT", "/v1/apps/df", appBody{Paused: &paused}), http.StatusOK, nil)

	if w := f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"}); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestWithdrawalValidation(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	tests := map[string]withdrawalBody{
		"no destination":  {AmountCents: "100"},
		"bad destination": {Destination: "not-an-address", AmountCents: "100"},
		"no amount":       {Destination: dest},
		"negative":        {Destination: dest, AmountCents: "-5"},
		"not a number":    {Destination: dest, AmountCents: "abc"},
		"fractional":      {Destination: dest, AmountCents: "1.5"},
	}
	for name, body := range tests {
		if w := f.do("POST", "/v1/apps/df/withdrawals", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, w.Code)
		}
	}
}

func TestWithdrawalsAreScopedToTheirApp(t *testing.T) {
	// One app must not be able to read another's withdrawal by guessing an id.
	f := newFixture(t)
	f.register("df")
	f.register("other")
	f.credit("df", 1000)

	var wd withdrawalView
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"}),
		http.StatusCreated, &wd)

	f.json(f.do("GET", "/v1/apps/df/withdrawals/"+wd.ID, nil), http.StatusOK, nil)
	if w := f.do("GET", "/v1/apps/other/withdrawals/"+wd.ID, nil); w.Code != http.StatusNotFound {
		t.Fatalf("cross-app read: status %d, want 404", w.Code)
	}
	if w := f.do("GET", "/v1/apps/df/withdrawals/not-a-uuid", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status %d, want 400", w.Code)
	}
	if w := f.do("GET", "/v1/apps/df/withdrawals/"+uuid.New().String(), nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status %d, want 404", w.Code)
	}
}

func TestListWithdrawalsByStatus(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	var open, settled withdrawalView
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"}), http.StatusCreated, &open)
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "200"}), http.StatusCreated, &settled)

	// Settle one, as the watcher would.
	if err := f.st.Update(func(tx *store.Tx) error {
		_, err := tx.MutateWithdrawal(uuid.MustParse(settled.ID), func(w *store.Withdrawal) error {
			w.Status = store.WithdrawalDone
			return nil
		})
		return err
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	var list struct {
		Withdrawals []withdrawalView `json:"withdrawals"`
	}
	f.json(f.do("GET", "/v1/apps/df/withdrawals", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 || list.Withdrawals[0].ID != open.ID {
		t.Fatalf("open set = %+v, want just the queued one", list.Withdrawals)
	}

	f.json(f.do("GET", "/v1/apps/df/withdrawals?status=done", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 1 || list.Withdrawals[0].ID != settled.ID {
		t.Fatalf("done = %+v", list.Withdrawals)
	}

	f.json(f.do("GET", "/v1/apps/df/withdrawals?status=all", nil), http.StatusOK, &list)
	if len(list.Withdrawals) != 2 {
		t.Fatalf("all = %d entries", len(list.Withdrawals))
	}

	if w := f.do("GET", "/v1/apps/df/withdrawals?status=nonsense", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", w.Code)
	}
}

func TestFeeSnapshotSurvivesAPolicyChange(t *testing.T) {
	// History has to stay explicable after an app edits its fee.
	f := newFixture(t)
	f.register("df")
	f.credit("df", 1000)

	var wd withdrawalView
	f.json(f.do("POST", "/v1/apps/df/withdrawals", withdrawalBody{Destination: dest, AmountCents: "100"}),
		http.StatusCreated, &wd)
	f.json(f.do("PUT", "/v1/apps/df", appBody{Fee: &feeBody{FlatCents: "999"}}), http.StatusOK, nil)

	if err := f.st.View(func(tx *store.Tx) error {
		got, _, err := tx.Withdrawal(uuid.MustParse(wd.ID))
		if err != nil {
			return err
		}
		if got.Fee != 1 {
			t.Fatalf("charged fee changed to %d", got.Fee)
		}
		if got.FeeSnapshot.Flat != 1 {
			t.Fatalf("snapshot = %d, want the policy at request time", got.FeeSnapshot.Flat)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}
