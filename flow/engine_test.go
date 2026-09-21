package flow

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

func addr(b byte) common.Address {
	var a common.Address
	a[common.AddressLength-1] = b
	return a
}

func hash(b byte) common.Hash {
	var h common.Hash
	h[common.HashLength-1] = b
	return h
}

func wei(v int64) *big.Int { return big.NewInt(v) }

var cfg = Config{DrainThreshold: wei(100)}

// fixture is the two-wallet shape most tests need: one that forwards, one that
// accumulates and can pay out. Under the old design these were an app's deposit
// address and its top-level; now the difference is one field (§32).
type fixture struct {
	t     *testing.T
	st    *store.Store
	hot   store.Wallet // drain_to unset: accumulates, pays out
	proxy store.Wallet // drain_to = hot: forwards
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, st: st}
	f.hot = store.Wallet{
		ID: 1, Ref: "hot", Address: addr(0x01),
		Active: true, Balance: new(big.Int), CreatedAt: now,
	}
	f.proxy = store.Wallet{
		ID: 2, Ref: "cust-1", Address: addr(0x02),
		DrainTo: f.hot.Address, Active: true, Balance: new(big.Int), CreatedAt: now,
	}
	f.update(func(tx *store.Tx) error {
		if err := tx.PutWallet(f.hot); err != nil {
			return err
		}
		return tx.PutWallet(f.proxy)
	})
	return f
}

func (f *fixture) update(fn func(*store.Tx) error) {
	f.t.Helper()
	if err := f.st.Update(fn); err != nil {
		f.t.Fatalf("Update: %v", err)
	}
}

func (f *fixture) wallet(id store.WalletID) store.Wallet {
	f.t.Helper()
	var w store.Wallet
	if err := f.st.View(func(tx *store.Tx) error {
		var ok bool
		var err error
		w, ok, err = tx.Wallet(id)
		if err != nil || !ok {
			f.t.Fatalf("wallet %s: ok=%v err=%v", id, ok, err)
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return w
}

// pending reads a promise back. It fails if the record has settled, which is
// the assertion most of these tests want: "is it still owed".
func (f *fixture) pending(id uuid.UUID) store.Pending {
	f.t.Helper()
	var p store.Pending
	if err := f.st.View(func(tx *store.Tx) error {
		var ok bool
		var err error
		p, ok, err = tx.Pending(id)
		if err != nil || !ok {
			f.t.Fatalf("pending %s: ok=%v err=%v", id, ok, err)
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// settled reads a fact back, and reports whether it is one yet.
func (f *fixture) settled(id uuid.UUID) (store.Withdrawal, bool) {
	f.t.Helper()
	var (
		wd store.Withdrawal
		ok bool
	)
	if err := f.st.View(func(tx *store.Tx) error {
		var err error
		wd, ok, err = tx.Withdrawal(id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return wd, ok
}

// credit puts money on a wallet the way the watcher would, and records the
// deposit that justifies it.
func (f *fixture) credit(w store.Wallet, block uint64, logIndex uint32, amount int64) {
	f.t.Helper()
	f.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(w.ID, wei(amount)); err != nil {
			return err
		}
		_, err := tx.PutDeposit(store.Deposit{
			Wallet: w.ID, Block: block, LogIndex: logIndex, TxHash: hash(byte(logIndex + 1)),
			From: addr(0xF0), Amount: wei(amount), CreatedAt: now,
		})
		return err
	})
}

func (f *fixture) newWithdrawal(amount int64, createdAt time.Time) store.Pending {
	f.t.Helper()
	p := store.Pending{
		ID: uuid.New(), Wallet: f.hot.ID, Reason: store.ReasonPayout,
		Destination: addr(0x99), Amount: wei(amount),
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	f.update(func(tx *store.Tx) error { return tx.PutPending(p) })
	return p
}

// start begins a flow and claims its wallet, then drives it to the state given.
func (f *fixture) start(p Params, state store.FlowState) store.Flow {
	f.t.Helper()
	var out store.Flow
	f.update(func(tx *store.Tx) error {
		fl, err := Begin(p)
		if err != nil {
			return err
		}
		fl.State = state
		fl.Tx = hash(0x77)
		if err := tx.PutFlow(fl); err != nil {
			return err
		}
		if _, err := tx.ClaimWallet(fl.Wallet, fl.ID); err != nil {
			return err
		}
		out = fl
		return nil
	})
	return out
}

func (f *fixture) advance(fl store.Flow, ok bool) store.Flow {
	f.t.Helper()
	var out store.Flow
	f.update(func(tx *store.Tx) error {
		var err error
		out, err = Advance(tx, fl, 42, ok, now)
		return err
	})
	return out
}

func (f *fixture) evaluate(w store.Wallet) bool {
	f.t.Helper()
	var started bool
	f.update(func(tx *store.Tx) error {
		var err error
		started, err = EvaluateWallet(tx, w, cfg, now)
		return err
	})
	return started
}

// --- drains ---

// A drain that lands records a debit and links to it the credits it carried.
// Nothing is credited anywhere: the records say where the money physically
// went, and bsc has no opinion about who is owed it (§34, §42).
func TestDrainRecordsADebitAndLinksItsCredits(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 500)
	f.credit(f.proxy, 10, 1, 300)

	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.proxy.ID, To: f.hot.Address, Active: true, Now: now,
	}, store.StateMoving)
	fl.Amount = wei(800) // resolved when the sweep was signed
	drain := fl.Tx
	f.advance(fl, true)

	if err := f.st.View(func(tx *store.Tx) error {
		// The drain produced a debit, born terminal and on the settled feed.
		settled, _, err := tx.SettledSince(store.Settled{}, 0)
		if err != nil {
			return err
		}
		if len(settled) != 1 {
			t.Fatalf("settled debits = %d, want the drain", len(settled))
		}
		debit := settled[0]
		if debit.Reason != store.ReasonDrain {
			t.Fatalf("debit = %+v, want a drain", debit)
		}
		if debit.TxHash != drain || debit.Amount.Cmp(wei(800)) != 0 || debit.Block != 42 {
			t.Fatalf("debit = %+v, want the sweep's hash, amount and block", debit)
		}

		// The credits are untouched. A drain moves a balance, not a set of
		// deposits, so nothing is stamped on them (§50) — the debit's amount is
		// the whole of what the movement says.
		all, err := tx.WalletDeposits(f.proxy.ID, 0)
		if err != nil {
			return err
		}
		if len(all) == 0 {
			t.Fatal("the credits that funded the drain are gone")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.wallet(f.proxy.ID); !got.Idle() {
		t.Fatal("the wallet was not released")
	}
}

func TestFailedDrainBacksTheWalletOff(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 500)
	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.proxy.ID, To: f.hot.Address, Active: true, Now: now,
	}, store.StateMoving)
	f.advance(fl, false)

	got := f.wallet(f.proxy.ID)
	if got.FailedAttempts != 1 {
		t.Fatalf("attempts = %d, want 1", got.FailedAttempts)
	}
	if !got.RetryAfter.After(now) {
		t.Fatal("no retry deadline was stamped")
	}
	if !got.Idle() {
		t.Fatal("the wallet was not released")
	}
	// And no debit was recorded: nothing left the wallet, so there is nothing
	// to observe. The balance is still there for the next evaluation to drain.
	if err := f.st.View(func(tx *store.Tx) error {
		settled, _, err := tx.SettledSince(store.Settled{}, 0)
		if err != nil {
			return err
		}
		if len(settled) != 0 {
			t.Fatalf("settled debits = %d after a failed drain, want none", len(settled))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- withdrawals ---

func TestConfirmedWithdrawalIsTerminalAndStopsBeingCommitted(t *testing.T) {
	f := newFixture(t)
	f.credit(f.hot, 10, 0, 1_000)
	wd := f.newWithdrawal(400, now)

	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.hot.ID, To: wd.Destination,
		Amount: wd.Amount, Withdrawal: wd.ID, Active: true, Now: now,
	}, store.StateMoving)
	f.advance(fl, true)

	// The promise became a fact: it left state/ and arrived in log/ (§51).
	got, ok := f.settled(wd.ID)
	if !ok {
		t.Fatal("the payout did not settle into the log")
	}
	if got.TxHash != fl.Tx {
		t.Fatalf("tx hash = %s, want %s", got.TxHash.Hex(), fl.Tx.Hex())
	}
	if err := f.st.View(func(tx *store.Tx) error {
		committed, err := tx.Committed(f.hot.ID)
		if err != nil {
			return err
		}
		if committed.Sign() != 0 {
			t.Fatalf("committed = %s after settlement, want 0", committed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A reverted payout is ours to retry, not the caller's to compensate for: the
// record stays pending — and so stays committed against the wallet — while the
// wallet backs off (§28).
func TestRevertedPayoutStaysPendingAndKeepsItsCommitment(t *testing.T) {
	f := newFixture(t)
	f.credit(f.hot, 10, 0, 1_000)
	wd := f.newWithdrawal(400, now)

	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.hot.ID, To: wd.Destination,
		Amount: wd.Amount, Withdrawal: wd.ID, Active: true, Now: now,
	}, store.StateMoving)
	fl.Error = "execution reverted"
	f.advance(fl, false)

	// It stays a promise: a reverted payout is not the caller's problem to
	// compensate for, so the record stays where the rules will retry it (§28).
	if _, settled := f.settled(wd.ID); settled {
		t.Fatal("a reverted payout was recorded as a fact")
	}
	got := f.pending(wd.ID)
	if got.Attempts != 1 || got.Error == "" {
		t.Fatalf("diagnostics not recorded: attempts=%d err=%q", got.Attempts, got.Error)
	}
	if err := f.st.View(func(tx *store.Tx) error {
		committed, err := tx.Committed(f.hot.ID)
		if err != nil {
			return err
		}
		if committed.Int64() != 400 {
			t.Fatalf("committed = %s, want the 400 still promised", committed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := f.wallet(f.hot.ID); w.FailedAttempts != 1 {
		t.Fatalf("wallet attempts = %d, want 1", w.FailedAttempts)
	}
}

// A withdrawal whose funding or approve failed never reached its payout, so it
// has not been decided either way.
func TestWithdrawalThatNeverPaidStaysPending(t *testing.T) {
	f := newFixture(t)
	f.credit(f.hot, 10, 0, 1_000)
	wd := f.newWithdrawal(400, now)

	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.hot.ID, To: wd.Destination,
		Amount: wd.Amount, Withdrawal: wd.ID, Active: false, Now: now,
	}, store.StateApproving)
	f.advance(fl, false)

	if _, settled := f.settled(wd.ID); settled {
		t.Fatal("a payout that never reached the transfer was recorded as a fact")
	}
	if w := f.wallet(f.hot.ID); w.FailedAttempts != 1 {
		t.Fatalf("wallet attempts = %d, want 1", w.FailedAttempts)
	}
}

// --- the work rules ---

func TestEvaluateStartsADrainOnAProxyWallet(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 500)

	if !f.evaluate(f.wallet(f.proxy.ID)) {
		t.Fatal("no drain started for a funded proxy wallet")
	}
	w := f.wallet(f.proxy.ID)
	if w.Idle() {
		t.Fatal("the wallet was not claimed")
	}
	if err := f.st.View(func(tx *store.Tx) error {
		fl, ok, err := tx.Flow(w.Flow)
		if err != nil || !ok {
			t.Fatalf("flow: ok=%v err=%v", ok, err)
		}
		if fl.Kind != store.FlowTransfer || fl.To != f.hot.Address {
			t.Fatalf("flow = %+v, want a drain to the hot wallet", fl)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateStartsNoDrainBelowTheThreshold(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 99) // threshold is 100

	if f.evaluate(f.wallet(f.proxy.ID)) {
		t.Fatal("a sub-threshold balance started a drain")
	}
}

func TestEvaluateStartsAWithdrawalOnAHotWallet(t *testing.T) {
	f := newFixture(t)
	f.credit(f.hot, 10, 0, 1_000)
	wd := f.newWithdrawal(400, now)

	if !f.evaluate(f.wallet(f.hot.ID)) {
		t.Fatal("no withdrawal started")
	}
	w := f.wallet(f.hot.ID)
	if err := f.st.View(func(tx *store.Tx) error {
		fl, ok, err := tx.Flow(w.Flow)
		if err != nil || !ok {
			t.Fatalf("flow: ok=%v err=%v", ok, err)
		}
		if fl.Kind != store.FlowTransfer || fl.Withdrawal != wd.ID {
			t.Fatalf("flow = %+v, want the pending withdrawal", fl)
		}
		if fl.Amount.Cmp(wd.Amount) != 0 {
			t.Fatalf("amount = %s, want %s", fl.Amount, wd.Amount)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Forwarding and paying out are mutually exclusive by construction, so a proxy
// wallet is never a payout candidate whatever is pending against it.
func TestEvaluateNeverPaysFromAProxyWallet(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 50) // below the drain threshold, so no drain either
	f.update(func(tx *store.Tx) error {
		return tx.PutPending(store.Pending{
			ID: uuid.New(), Wallet: f.proxy.ID, Reason: store.ReasonPayout,
			Destination: addr(0x99), Amount: wei(10),
			CreatedAt: now,
		})
	})

	if f.evaluate(f.wallet(f.proxy.ID)) {
		t.Fatal("a payout was started from a forwarding wallet")
	}
}

func TestEvaluateStartsNothingWhileTheWalletIsBusy(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 500)
	f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.proxy.ID, To: f.hot.Address, Active: true, Now: now,
	}, store.StateMoving)

	if f.evaluate(f.wallet(f.proxy.ID)) {
		t.Fatal("a second flow started on a busy wallet")
	}
}

// Oldest first, so a payout that keeps reverting cannot starve the queue behind
// it forever.
func TestOldestPendingWithdrawalGoesFirst(t *testing.T) {
	f := newFixture(t)
	f.credit(f.hot, 10, 0, 10_000)
	older := f.newWithdrawal(100, now.Add(-time.Hour))
	f.newWithdrawal(200, now)

	if !f.evaluate(f.wallet(f.hot.ID)) {
		t.Fatal("no withdrawal started")
	}
	w := f.wallet(f.hot.ID)
	if err := f.st.View(func(tx *store.Tx) error {
		fl, _, err := tx.Flow(w.Flow)
		if err != nil {
			return err
		}
		if fl.Withdrawal != older.ID {
			t.Fatal("the newer withdrawal was started first")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Because the rules read current state rather than react to events, one sweep
// converges whatever was missed while the process was down.
func TestEvaluateAllConvergesAtStartup(t *testing.T) {
	f := newFixture(t)
	f.credit(f.proxy, 10, 0, 500)
	f.credit(f.hot, 10, 1, 1_000)
	f.newWithdrawal(400, now)

	var started int
	f.update(func(tx *store.Tx) error {
		var err error
		started, err = EvaluateAll(tx, cfg, now)
		return err
	})
	if started != 2 {
		t.Fatalf("EvaluateAll started %d flows, want 2 (a drain and a payout)", started)
	}
	p, h := f.wallet(f.proxy.ID), f.wallet(f.hot.ID)
	if p.Idle() || h.Idle() {
		t.Fatal("a wallet with work is still idle")
	}
}

func TestAdvanceRefusesATerminalFlow(t *testing.T) {
	f := newFixture(t)
	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.proxy.ID, To: f.hot.Address, Active: true, Now: now,
	}, store.StateMoving)
	fl.State = store.StateDone

	err := f.st.Update(func(tx *store.Tx) error {
		_, err := Advance(tx, fl, 42, true, now)
		return err
	})
	if err == nil {
		t.Fatal("Advance accepted a terminal flow")
	}
}

// Leaving `approving` successfully is what makes a wallet active, whichever
// flow happened to be the one that activated it.
func TestApprovingMarksTheWalletActive(t *testing.T) {
	f := newFixture(t)
	f.update(func(tx *store.Tx) error {
		_, err := tx.SetActive(f.proxy.ID, false)
		return err
	})
	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.proxy.ID, To: f.hot.Address, Active: false, Now: now,
	}, store.StateApproving)
	f.advance(fl, true)

	if got := f.wallet(f.proxy.ID); !got.Active {
		t.Fatal("the wallet was not marked active")
	}
}

// A drain that needed no transaction — the sender found the wallet empty and
// skipped — moved nothing, so there is nothing to observe and no debit.
func TestSkippedDrainRecordsNoDebit(t *testing.T) {
	f := newFixture(t)
	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.proxy.ID, To: f.hot.Address, Active: true, Now: now,
	}, store.StateMoving)
	fl.Tx = common.Hash{} // nothing was broadcast
	f.advance(fl, true)

	if err := f.st.View(func(tx *store.Tx) error {
		settled, _, err := tx.SettledSince(store.Settled{}, 0)
		if err != nil {
			return err
		}
		if len(settled) != 0 {
			t.Fatalf("recorded %d debits for a drain that moved nothing", len(settled))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A confirmed payout joins the same feed the drain does, which is the point:
// one feed of everything that left, whoever decided it (§42).
func TestSettledFeedCarriesBothKinds(t *testing.T) {
	f := newFixture(t)
	f.credit(f.hot, 10, 0, 1_000)
	wd := f.newWithdrawal(400, now)
	fl := f.start(Params{
		Kind: store.FlowTransfer, Wallet: f.hot.ID, To: wd.Destination,
		Amount: wd.Amount, Withdrawal: wd.ID, Active: true, Now: now,
	}, store.StateMoving)
	f.advance(fl, true)

	if err := f.st.View(func(tx *store.Tx) error {
		settled, cursor, err := tx.SettledSince(store.Settled{}, 0)
		if err != nil {
			return err
		}
		if len(settled) != 1 || settled[0].Reason != store.ReasonPayout {
			t.Fatalf("settled = %+v, want the payout", settled)
		}
		if settled[0].Block != 42 {
			t.Fatalf("block = %d, want the one it settled in", settled[0].Block)
		}
		// Passing the cursor back yields nothing new, so a caller that keeps
		// passing it never loses its place.
		again, next, err := tx.SettledSince(cursor, 0)
		if err != nil {
			return err
		}
		if len(again) != 0 || next != cursor {
			t.Fatalf("cursor moved with nothing new: %v -> %v", cursor, next)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
