package api

import (
	"math/big"
	"net/http"
	"testing"
	"time"

	"bsc/store"
)

type depositsPage struct {
	Deposits []depositView `json:"deposits"`
	Cursor   string        `json:"cursor"`
}

// deposit records an incoming transfer on a wallet, as the watcher would.
func (f *fixture) deposit(ref string, block uint64, logIndex uint32, amount int64) {
	f.t.Helper()
	if err := f.st.Update(func(tx *store.Tx) error {
		w, ok, err := tx.WalletByRef(ref)
		if err != nil || !ok {
			f.t.Fatalf("wallet %q: ok=%v err=%v", ref, ok, err)
		}
		if _, err := tx.Credit(w.ID, big.NewInt(amount)); err != nil {
			return err
		}
		var txh [32]byte
		txh[31] = byte(block)
		txh[30] = byte(logIndex)
		_, err = tx.PutDeposit(store.Deposit{
			Wallet: w.ID, Block: block, LogIndex: logIndex, TxHash: txh,
			From: hexAddr(0xF0), Amount: big.NewInt(amount),
			Status: store.DepositReceived, CreatedAt: time.Now().UTC(),
		})
		return err
	}); err != nil {
		f.t.Fatalf("deposit: %v", err)
	}
}

// Deposits are unsolicited — nobody can know one is coming — so the caller asks
// "what is new since I last looked".
func TestDepositFeedIsOrderedAndResumable(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.proxy("cust-1", hot.Address)
	f.deposit("cust-1", 10, 0, 100)
	f.deposit("cust-1", 20, 0, 200)
	f.deposit("cust-1", 30, 0, 300)

	var page depositsPage
	f.json(f.do("GET", "/v1/deposits?limit=2", nil), http.StatusOK, &page)
	if len(page.Deposits) != 2 {
		t.Fatalf("page = %d deposits, want 2", len(page.Deposits))
	}
	if page.Deposits[0].Amount != "100" || page.Deposits[1].Amount != "200" {
		t.Fatalf("out of order: %+v", page.Deposits)
	}

	var next depositsPage
	f.json(f.do("GET", "/v1/deposits?since="+page.Cursor, nil), http.StatusOK, &next)
	if len(next.Deposits) != 1 || next.Deposits[0].Amount != "300" {
		t.Fatalf("resume = %+v", next.Deposits)
	}

	// Passing the cursor back when nothing is new returns it unchanged, so a
	// caller that keeps passing it never loses its place.
	var empty depositsPage
	f.json(f.do("GET", "/v1/deposits?since="+next.Cursor, nil), http.StatusOK, &empty)
	if len(empty.Deposits) != 0 || empty.Cursor != next.Cursor {
		t.Fatalf("cursor moved with nothing new: %q -> %q", next.Cursor, empty.Cursor)
	}
}

// The feed is global: one caller owns this service, and splitting it per app
// was a tenancy boundary bsc no longer draws (§37).
func TestDepositFeedSpansEveryWallet(t *testing.T) {
	f := newFixture(t)
	f.wallet("a")
	f.wallet("b")
	f.deposit("a", 10, 0, 100)
	f.deposit("b", 11, 0, 200)

	var all depositsPage
	f.json(f.do("GET", "/v1/deposits", nil), http.StatusOK, &all)
	if len(all.Deposits) != 2 {
		t.Fatalf("feed = %d, want 2", len(all.Deposits))
	}

	var one depositsPage
	f.json(f.do("GET", "/v1/wallets/a/deposits", nil), http.StatusOK, &one)
	if len(one.Deposits) != 1 || one.Deposits[0].Wallet != "a" {
		t.Fatalf("per-wallet view = %+v", one.Deposits)
	}
}

// The cursor is opaque but ordered: a caller may compare two ids and pass one
// back, never parse one.
func TestDepositIdIsTheCursor(t *testing.T) {
	f := newFixture(t)
	f.wallet("a")
	f.deposit("a", 10, 4, 100)

	var page depositsPage
	f.json(f.do("GET", "/v1/deposits", nil), http.StatusOK, &page)
	if len(page.Deposits) != 1 {
		t.Fatalf("deposits = %d", len(page.Deposits))
	}
	if page.Deposits[0].ID != page.Cursor {
		t.Fatalf("id %q != cursor %q", page.Deposits[0].ID, page.Cursor)
	}
	if w := f.do("GET", "/v1/deposits?since=not-a-cursor", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor: status %d, want 400", w.Code)
	}
}

func TestWalletDepositsFilterByStatus(t *testing.T) {
	f := newFixture(t)
	hot := f.wallet("hot")
	f.proxy("cust-1", hot.Address)
	f.deposit("cust-1", 10, 0, 100)

	var got depositsPage
	f.json(f.do("GET", "/v1/wallets/cust-1/deposits?status=received", nil), http.StatusOK, &got)
	if len(got.Deposits) != 1 {
		t.Fatalf("received = %d, want 1", len(got.Deposits))
	}
	f.json(f.do("GET", "/v1/wallets/cust-1/deposits?status=forwarded", nil), http.StatusOK, &got)
	if len(got.Deposits) != 0 {
		t.Fatalf("forwarded = %d, want 0", len(got.Deposits))
	}
	if w := f.do("GET", "/v1/wallets/cust-1/deposits?status=nonsense", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d, want 400", w.Code)
	}
}
