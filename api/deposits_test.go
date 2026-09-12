package api

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	"bsc/money"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

type depositsPage struct {
	Deposits []depositView `json:"deposits"`
	Cursor   string        `json:"cursor"`
}

// deposit records one, as the watcher would after seeing a Transfer log.
func (f *fixture) deposit(slug, ref string, block uint64, logIndex uint32, amount int64) {
	f.t.Helper()
	if err := f.st.Update(func(tx *store.Tx) error {
		w, ok, err := tx.WalletByRef(slug, ref)
		if err != nil || !ok {
			f.t.Fatalf("wallet for ref %q: ok=%v err=%v", ref, ok, err)
		}
		var txHash common.Hash
		txHash[0], txHash[1] = byte(block), byte(logIndex)
		_, err = tx.PutDeposit(store.Deposit{
			Wallet: w.ID, App: slug, Block: block, LogIndex: logIndex, TxHash: txHash,
			From: common.HexToAddress("0xf0"), AmountWei: big.NewInt(amount), Cents: money.Cents(amount),
			Status: store.DepositConfirmed, CreatedAt: time.Now().UTC(),
		})
		return err
	}); err != nil {
		f.t.Fatalf("deposit: %v", err)
	}
}

func TestDepositFeedIsCursored(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/wallets", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.deposit("df", "cust-1", 100, 3, 500)
	f.deposit("df", "cust-1", 101, 0, 600)

	var page depositsPage
	f.json(f.do("GET", "/v1/apps/df/deposits?limit=1", nil), http.StatusOK, &page)
	if len(page.Deposits) != 1 || page.Deposits[0].Block != 100 {
		t.Fatalf("first page = %+v", page.Deposits)
	}
	// The cursor is the chain's own ordering, and the app sees its own ref
	// rather than an address it never chose.
	if page.Deposits[0].Cursor != "100-3" || page.Deposits[0].Ref != "cust-1" {
		t.Fatalf("deposit view = %+v", page.Deposits[0])
	}
	if page.Cursor != "100-3" {
		t.Fatalf("cursor = %q", page.Cursor)
	}

	f.json(f.do("GET", "/v1/apps/df/deposits?since="+page.Cursor, nil), http.StatusOK, &page)
	if len(page.Deposits) != 1 || page.Deposits[0].Block != 101 {
		t.Fatalf("second page = %+v", page.Deposits)
	}

	// Passing the cursor back when nothing is new must not lose the place.
	prev := page.Cursor
	f.json(f.do("GET", "/v1/apps/df/deposits?since="+prev, nil), http.StatusOK, &page)
	if len(page.Deposits) != 0 || page.Cursor != prev {
		t.Fatalf("empty read moved the cursor: %q -> %q", prev, page.Cursor)
	}
}

func TestDepositFeedIsScopedPerApp(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.register("other")
	f.json(f.do("POST", "/v1/apps/df/wallets", depositAddressBody{Ref: "a"}), http.StatusCreated, nil)
	f.json(f.do("POST", "/v1/apps/other/wallets", depositAddressBody{Ref: "a"}), http.StatusCreated, nil)
	f.deposit("df", "a", 100, 0, 1)
	f.deposit("other", "a", 101, 0, 2)

	var page depositsPage
	f.json(f.do("GET", "/v1/apps/df/deposits", nil), http.StatusOK, &page)
	if len(page.Deposits) != 1 || page.Deposits[0].Block != 100 {
		t.Fatalf("df feed = %+v", page.Deposits)
	}
}

func TestUncreditedDepositsAreListable(t *testing.T) {
	// Crediting is a status rather than a feed entry, because one sweep credits
	// several deposits at once.
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/wallets", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.deposit("df", "cust-1", 100, 0, 300)
	f.deposit("df", "cust-1", 100, 1, 400)

	var page depositsPage
	f.json(f.do("GET", "/v1/apps/df/deposits?status=confirmed", nil), http.StatusOK, &page)
	if len(page.Deposits) != 2 {
		t.Fatalf("awaiting drain = %d, want 2", len(page.Deposits))
	}

	// Credit them, as a landed sweep would.
	var wallet uuid.UUID
	if err := f.st.Update(func(tx *store.Tx) error {
		w, _, err := tx.WalletByRef("df", "cust-1")
		if err != nil {
			return err
		}
		wallet = w.ID
		_, cents, err := tx.CreditDeposits("df", wallet, common.HexToHash("0xaa"))
		if err != nil {
			return err
		}
		_, err = tx.CreditLedger("df", cents)
		return err
	}); err != nil {
		t.Fatalf("credit: %v", err)
	}

	f.json(f.do("GET", "/v1/apps/df/deposits?status=confirmed", nil), http.StatusOK, &page)
	if len(page.Deposits) != 0 {
		t.Fatalf("still awaiting drain: %+v", page.Deposits)
	}
	f.json(f.do("GET", "/v1/apps/df/deposits", nil), http.StatusOK, &page)
	for _, d := range page.Deposits {
		if d.Status != "credited" || d.DrainTx == "" {
			t.Fatalf("deposit = %+v, want credited and stamped", d)
		}
	}
}

func TestDepositQueryValidation(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	for _, q := range []string{"?since=nonsense", "?status=weird", "?limit=0", "?limit=abc"} {
		if w := f.do("GET", "/v1/apps/df/deposits"+q, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", q, w.Code)
		}
	}
}

func TestPendingBalanceCountsUndrainedDeposits(t *testing.T) {
	// Money detected but not yet swept is real and worth showing, but it is not
	// spendable until it reaches the wallet payouts are drawn from.
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/wallets", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.deposit("df", "cust-1", 10, 0, 750)

	var app appView
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, &app)
	if app.Balance.PendingCents != "750" {
		t.Fatalf("pending = %s, want 750", app.Balance.PendingCents)
	}
	if app.Balance.AvailableCents != "0" {
		t.Fatalf("available = %s: undrained money must not be spendable", app.Balance.AvailableCents)
	}
}

// TestPendingExcludesUncreditedDust: a deposit wallet also collects sub-cent
// remainders that were never credited to anyone. Reporting them as the app's
// pending money would promise a balance that will never arrive (§22).
func TestPendingExcludesUncreditedDust(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.json(f.do("POST", "/v1/apps/df/wallets", depositAddressBody{Ref: "cust-1"}), http.StatusCreated, nil)
	f.deposit("df", "cust-1", 10, 0, 750)
	// Custody rises by more than was ever recorded, as flooring guarantees.
	if err := f.st.Update(func(tx *store.Tx) error {
		w, _, err := tx.WalletByRef("df", "cust-1")
		if err != nil {
			return err
		}
		_, err = tx.Credit(w.ID, big.NewInt(999))
		return err
	}); err != nil {
		t.Fatalf("credit: %v", err)
	}

	var app appView
	f.json(f.do("GET", "/v1/apps/df", nil), http.StatusOK, &app)
	if app.Balance.PendingCents != "750" {
		t.Fatalf("pending = %s, want only the recorded deposits", app.Balance.PendingCents)
	}
}

func TestBalanceEndpoint(t *testing.T) {
	f := newFixture(t)
	f.register("df")
	f.credit("df", 500)

	var b balanceView
	f.json(f.do("GET", "/v1/apps/df/balance", nil), http.StatusOK, &b)
	if b.AvailableCents != "500" || b.ReservedCents != "0" || b.PendingCents != "0" {
		t.Fatalf("balance = %+v", b)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	if w := f.do("GET", "/healthz", nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}
