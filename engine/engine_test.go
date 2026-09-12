package engine

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"bsc/flow"
	"bsc/money"
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

var now = time.Now().UTC()

// cfg values a cent at a wei, which is the truth for a two-decimal token and
// keeps every figure in these tests readable as both.
var scale = mustScale()

func mustScale() money.Scale {
	sc, err := money.NewScale(2)
	if err != nil {
		panic(err)
	}
	return sc
}

var cfg = Config{
	Scale:          scale,
	DrainThreshold: wei(100),
	HouseSweepMin:  wei(100),
	FeeCollector:   addr(0xFE),
}

type fixture struct {
	t   *testing.T
	st  *store.Store
	app store.App
	top store.Wallet
	dep store.Wallet
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bsc.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, st: st}
	f.top = store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindTopLevel, Address: addr(0x01),
		Active: true, Balance: new(big.Int), CreatedAt: now,
	}
	f.dep = store.Wallet{
		ID: uuid.New(), App: "df", Kind: store.KindDeposit, Ref: "cust-1", Address: addr(0x02),
		Active: true, Balance: new(big.Int), CreatedAt: now,
	}
	f.app = store.App{Slug: "df", Wallet: f.top.ID, CreatedAt: now}
	f.update(func(tx *store.Tx) error {
		if err := tx.PutWallet(f.top); err != nil {
			return err
		}
		if err := tx.PutWallet(f.dep); err != nil {
			return err
		}
		return tx.PutApp(f.app)
	})
	return f
}

func (f *fixture) update(fn func(*store.Tx) error) {
	f.t.Helper()
	if err := f.st.Update(fn); err != nil {
		f.t.Fatalf("Update: %v", err)
	}
}

func (f *fixture) wallet(id uuid.UUID) store.Wallet {
	f.t.Helper()
	var out store.Wallet
	if err := f.st.View(func(tx *store.Tx) error {
		w, ok, err := tx.Wallet(id)
		if err != nil || !ok {
			f.t.Fatalf("wallet: ok=%v err=%v", ok, err)
		}
		out = w
		return nil
	}); err != nil {
		f.t.Fatalf("View: %v", err)
	}
	return out
}

func (f *fixture) flows() []store.Flow {
	f.t.Helper()
	var out []store.Flow
	if err := f.st.View(func(tx *store.Tx) error {
		return tx.EachFlow(func(fl store.Flow) error {
			out = append(out, fl)
			return nil
		})
	}); err != nil {
		f.t.Fatalf("View: %v", err)
	}
	return out
}

// withdrawalFlow puts a withdrawal in flight at the given state.
func (f *fixture) withdrawalFlow(wd store.Withdrawal, state store.FlowState) store.Flow {
	f.t.Helper()
	fl, err := flow.Begin(flow.Params{
		Kind: store.FlowWithdrawal, Wallet: f.top.ID, App: "df",
		To: wd.Destination, Amount: scale.Wei(wd.Payout), Withdrawal: wd.ID,
		Active: true, Now: now,
	})
	if err != nil {
		f.t.Fatalf("Begin: %v", err)
	}
	fl.State = state
	fl.Tx = hash(0x71)
	f.update(func(tx *store.Tx) error {
		if err := tx.PutFlow(fl); err != nil {
			return err
		}
		_, err := tx.ClaimWallet(f.top.ID, fl.ID)
		return err
	})
	return fl
}

// chargedWithdrawal creates a queued withdrawal with payout and fee reserved as
// one figure, exactly as the API does.
func (f *fixture) chargedWithdrawal(payout, fee money.Cents) store.Withdrawal {
	f.t.Helper()
	wd := store.Withdrawal{
		ID: uuid.New(), App: "df", Destination: addr(0xDD),
		Amount: payout, Fee: fee, Payout: payout, Debit: payout + fee,
		Status: store.WithdrawalQueued, CreatedAt: now,
	}
	f.update(func(tx *store.Tx) error {
		if err := tx.PutWithdrawal(wd); err != nil {
			return err
		}
		_, err := tx.ReserveLedger("df", wd.Debit)
		return err
	})
	return wd
}

// fund gives the app a ledger balance and the matching custody, the state every
// money test starts from.
func (f *fixture) fund(cents money.Cents) {
	f.t.Helper()
	f.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(f.top.ID, scale.Wei(cents)); err != nil {
			return err
		}
		_, err := tx.CreditLedger("df", cents)
		return err
	})
}

// appState reads the app back, for asserting on its ledger.
func (f *fixture) appState() store.App {
	f.t.Helper()
	var out store.App
	if err := f.st.View(func(tx *store.Tx) error {
		a, ok, err := tx.App("df")
		if err != nil || !ok {
			f.t.Fatalf("App: ok=%v err=%v", ok, err)
		}
		out = a
		return nil
	}); err != nil {
		f.t.Fatalf("View: %v", err)
	}
	return out
}

// TestWithdrawalDebitsPayoutAndFeeTogether is the heart of the fee model: both
// leave the ledger when the payout confirms, but only the payout moves on-chain.
// The difference stays in the wallet as the house's (§24).
func TestWithdrawalDebitsPayoutAndFeeTogether(t *testing.T) {
	f := newFixture(t)
	f.fund(1000)
	wd := f.chargedWithdrawal(50, 1)
	fl := f.withdrawalFlow(wd, store.StatePaying)

	f.update(func(tx *store.Tx) error {
		got, err := Advance(tx, fl, true, now)
		if err != nil {
			return err
		}
		if got.State != store.StateDone {
			t.Fatalf("flow state = %s, want done: a withdrawal is one transfer", got.State)
		}
		return nil
	})

	if err := f.st.View(func(tx *store.Tx) error {
		got, _, err := tx.Withdrawal(wd.ID)
		if err != nil {
			return err
		}
		if got.Status != store.WithdrawalDone {
			t.Fatalf("withdrawal = %s, want done as soon as the payout confirmed", got.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}

	a := f.appState()
	if a.Ledger != 949 {
		t.Fatalf("ledger = %d, want 1000-50-1", a.Ledger)
	}
	if a.Reserved != 0 {
		t.Fatalf("reserved = %d after settlement, want 0", a.Reserved)
	}
	// The fee never moved: custody still holds it, and it is now ours.
	if got := f.wallet(f.top.ID).Balance; got.Cmp(wei(1000)) != 0 {
		t.Fatalf("custody = %s; the engine must not move tokens itself", got)
	}
	if excess := scale.Excess(wei(1000), a.Ledger); excess.Cmp(wei(51)) != 0 {
		t.Fatalf("house excess = %s, want the payout still to leave plus the fee", excess)
	}
}

// TestFailedPayoutReleasesWithoutDebiting: a payout that reverted charges
// nothing, fee included. The app gets its whole reservation back.
func TestFailedPayoutReleasesWithoutDebiting(t *testing.T) {
	f := newFixture(t)
	f.fund(1000)
	wd := f.chargedWithdrawal(50, 3)
	fl := f.withdrawalFlow(wd, store.StatePaying)

	f.update(func(tx *store.Tx) error {
		_, err := Advance(tx, fl, false, now)
		return err
	})

	a := f.appState()
	if a.Ledger != 1000 || a.Reserved != 0 {
		t.Fatalf("ledger %d reserved %d, want the whole reservation returned", a.Ledger, a.Reserved)
	}
	if err := f.st.View(func(tx *store.Tx) error {
		got, _, err := tx.Withdrawal(wd.ID)
		if err != nil {
			return err
		}
		if got.Status != store.WithdrawalFailed {
			t.Fatalf("withdrawal = %s, want failed", got.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestWithdrawalThatNeverPaidStaysQueued: a failure during activation has not
// decided the request, so it stays queued for the rules to retry — behind a
// backoff, or the retry is a loop that burns gas as fast as blocks arrive.
func TestWithdrawalThatNeverPaidStaysQueued(t *testing.T) {
	f := newFixture(t)
	f.fund(1000)
	wd := f.chargedWithdrawal(50, 1)
	fl := f.withdrawalFlow(wd, store.StateApproving)

	f.update(func(tx *store.Tx) error {
		_, err := Advance(tx, fl, false, now)
		return err
	})

	if err := f.st.View(func(tx *store.Tx) error {
		got, _, err := tx.Withdrawal(wd.ID)
		if err != nil {
			return err
		}
		if got.Status != store.WithdrawalQueued {
			t.Fatalf("withdrawal = %s, want it still queued for a retry", got.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
	// Still reserved: the money is still committed to this request.
	if a := f.appState(); a.Reserved != 51 {
		t.Fatalf("reserved = %d, want the request still holding its money", a.Reserved)
	}
	w := f.wallet(f.top.ID)
	if !w.Idle() {
		t.Fatalf("wallet still held by %s", w.Flow)
	}
	if w.FailedAttempts == 0 || w.RetryAfter.IsZero() {
		t.Fatal("a failed withdrawal left no backoff, so the rule will re-fire immediately")
	}
}

// TestDrainCreditsTheLedgerOnlyWhenItLands: money in a deposit address cannot be
// paid out of the hot wallet, so it is not spendable until the sweep confirms.
func TestDrainCreditsTheLedgerOnlyWhenItLands(t *testing.T) {
	f := newFixture(t)
	f.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(f.dep.ID, wei(500)); err != nil {
			return err
		}
		_, err := tx.PutDeposit(store.Deposit{
			Wallet: f.dep.ID, App: "df", Block: 10, LogIndex: 0, TxHash: hash(0x51),
			From: addr(0xF0), AmountWei: wei(500), Cents: 500,
			Status: store.DepositConfirmed, CreatedAt: now,
		})
		return err
	})
	if a := f.appState(); a.Ledger != 0 {
		t.Fatalf("ledger = %d before the drain landed, want 0", a.Ledger)
	}

	fl, err := flow.Begin(flow.Params{
		Kind: store.FlowDrain, Wallet: f.dep.ID, App: "df",
		To: f.top.Address, Active: true, Now: now,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	fl.State, fl.Tx = store.StateSweeping, hash(0x52)
	f.update(func(tx *store.Tx) error {
		if err := tx.PutFlow(fl); err != nil {
			return err
		}
		_, err := tx.ClaimWallet(f.dep.ID, fl.ID)
		return err
	})

	f.update(func(tx *store.Tx) error {
		_, err := Advance(tx, fl, true, now)
		return err
	})
	if a := f.appState(); a.Ledger != 500 {
		t.Fatalf("ledger = %d after the sweep landed, want 500", a.Ledger)
	}
}

// TestHouseSweepStartsOnlyOnTheExcess covers the rule that collects fees and
// dust: it must see what the wallet holds beyond the ledger, and nothing more.
func TestHouseSweepStartsOnlyOnTheExcess(t *testing.T) {
	f := newFixture(t)
	f.fund(1000) // custody 1000 wei, ledger 1000 cents — no excess

	f.update(func(tx *store.Tx) error {
		started, err := EvaluateApp(tx, f.appState(), cfg, now)
		if err != nil {
			return err
		}
		if started {
			t.Fatal("swept an app whose wallet holds exactly what it is owed")
		}
		return nil
	})

	// A fee's worth arrives without anybody being credited for it.
	f.update(func(tx *store.Tx) error {
		_, err := tx.Credit(f.top.ID, wei(150))
		return err
	})
	f.update(func(tx *store.Tx) error {
		started, err := EvaluateApp(tx, f.appState(), cfg, now)
		if err != nil {
			return err
		}
		if !started {
			t.Fatal("excess above the threshold was not swept")
		}
		return nil
	})

	got := f.flows()
	if len(got) != 1 || got[0].Kind != store.FlowHouseSweep {
		t.Fatalf("flows = %+v, want one house sweep", got)
	}
	if got[0].To != cfg.FeeCollector {
		t.Fatalf("sweeping to %s, want the collector", got[0].To.Hex())
	}
	// The amount is resolved at signing time, not fixed here.
	if got[0].Amount != nil && got[0].Amount.Sign() != 0 {
		t.Fatalf("house sweep fixed an amount of %s", got[0].Amount)
	}
}

// TestPayoutOutranksTheHouseSweep: one wallet runs one flow at a time, and the
// app's money is the more urgent use of it.
func TestPayoutOutranksTheHouseSweep(t *testing.T) {
	f := newFixture(t)
	f.fund(1000)
	f.update(func(tx *store.Tx) error {
		_, err := tx.Credit(f.top.ID, wei(5000)) // plenty of excess
		return err
	})
	f.chargedWithdrawal(50, 1)

	f.update(func(tx *store.Tx) error {
		_, err := EvaluateApp(tx, f.appState(), cfg, now)
		return err
	})
	got := f.flows()
	if len(got) != 1 || got[0].Kind != store.FlowWithdrawal {
		t.Fatalf("flows = %+v, want the payout to go first", got)
	}
}

func TestOldestQueuedWithdrawalGoesFirst(t *testing.T) {
	f := newFixture(t)
	var first uuid.UUID
	f.update(func(tx *store.Tx) error {
		if _, err := tx.CreditLedger("df", 1000); err != nil {
			return err
		}
		if _, err := tx.Credit(f.top.ID, wei(1000)); err != nil {
			return err
		}
		for i, age := range []time.Duration{-time.Minute, -time.Hour} {
			wd := store.Withdrawal{
				ID: uuid.New(), App: "df", Destination: addr(byte(0xD0 + i)),
				Amount: 10, Payout: 10, Debit: 10,
				Status: store.WithdrawalQueued, CreatedAt: now.Add(age),
			}
			if age == -time.Hour {
				first = wd.ID
			}
			if err := tx.PutWithdrawal(wd); err != nil {
				return err
			}
			if _, err := tx.ReserveLedger("df", wd.Debit); err != nil {
				return err
			}
		}
		return nil
	})

	f.update(func(tx *store.Tx) error {
		_, err := EvaluateApp(tx, f.app, cfg, now)
		return err
	})

	got := f.flows()
	if len(got) != 1 || got[0].Withdrawal != first {
		t.Fatalf("started %+v, want the oldest queued withdrawal %s", got, first)
	}
}

func TestEvaluateAppStartsNothingWhileTheWalletIsBusy(t *testing.T) {
	f := newFixture(t)
	f.update(func(tx *store.Tx) error {
		if _, err := tx.CreditLedger("df", 1000); err != nil {
			return err
		}
		if _, err := tx.Credit(f.top.ID, wei(1000)); err != nil {
			return err
		}
		wd := store.Withdrawal{
			ID: uuid.New(), App: "df", Destination: addr(0xDD),
			Amount: 10, Payout: 10, Debit: 10,
			Status: store.WithdrawalQueued, CreatedAt: now,
		}
		if err := tx.PutWithdrawal(wd); err != nil {
			return err
		}
		if _, err := tx.ReserveLedger("df", wd.Debit); err != nil {
			return err
		}
		_, err := tx.ClaimWallet(f.top.ID, uuid.New()) // something else owns it
		return err
	})

	f.update(func(tx *store.Tx) error {
		started, err := EvaluateApp(tx, f.app, cfg, now)
		if err != nil {
			return err
		}
		if started {
			t.Fatal("started work on a wallet another flow owns")
		}
		return nil
	})
}

func TestEvaluateAllConvergesAtStartup(t *testing.T) {
	// The rules read current state rather than react to events, so one sweep
	// picks up everything missed while the process was down.
	f := newFixture(t)
	f.update(func(tx *store.Tx) error {
		if _, err := tx.Credit(f.dep.ID, wei(500)); err != nil { // deposit awaiting a drain
			return err
		}
		if _, err := tx.CreditLedger("df", 1000); err != nil {
			return err
		}
		if _, err := tx.Credit(f.top.ID, wei(1000)); err != nil {
			return err
		}
		wd := store.Withdrawal{
			ID: uuid.New(), App: "df", Destination: addr(0xDD),
			Amount: 10, Payout: 10, Debit: 10,
			Status: store.WithdrawalQueued, CreatedAt: now,
		}
		if err := tx.PutWithdrawal(wd); err != nil {
			return err
		}
		_, err := tx.ReserveLedger("df", wd.Debit)
		return err
	})

	var started int
	f.update(func(tx *store.Tx) error {
		var err error
		started, err = EvaluateAll(tx, cfg, now)
		return err
	})
	if started != 2 {
		t.Fatalf("started %d flows, want a drain and a withdrawal", started)
	}
	kinds := map[store.FlowKind]bool{}
	for _, fl := range f.flows() {
		kinds[fl.Kind] = true
	}
	if !kinds[store.FlowDrain] || !kinds[store.FlowWithdrawal] {
		t.Fatalf("kinds = %v", kinds)
	}
}

func TestAdvanceRefusesATerminalFlow(t *testing.T) {
	f := newFixture(t)
	fl := store.Flow{ID: uuid.New(), Kind: store.FlowDrain, State: store.StateDone, App: "df", Wallet: f.dep.ID}
	err := f.st.Update(func(tx *store.Tx) error {
		_, err := Advance(tx, fl, true, now)
		return err
	})
	if err == nil {
		t.Fatal("advanced a finished flow")
	}
}

// masterFixture records the master wallet, which a gas top-up owns.
func (f *fixture) master(tokens int64) store.Wallet {
	f.t.Helper()
	w := store.Wallet{
		ID: uuid.New(), Kind: store.KindMaster, Address: addr(0x99),
		Balance: wei(tokens), CreatedAt: now,
	}
	f.update(func(tx *store.Tx) error { return tx.PutWallet(w) })
	return w
}

var gasCfg = Config{
	Scale:          scale,
	DrainThreshold: wei(100),
	FeeCollector:   addr(0xFE),
	SwapEnabled:    true,
	SwapAmount:     wei(10),
	GasFloor:       wei(100),
	SwapCooldown:   time.Hour,
}

func TestEvaluateGasStartsATopUpAndArmsTheCooldown(t *testing.T) {
	f := newFixture(t)
	m := f.master(50)

	f.update(func(tx *store.Tx) error {
		started, err := EvaluateGas(tx, gasCfg, m, wei(5), now) // well below the floor
		if err != nil {
			return err
		}
		if !started {
			t.Fatal("no top-up started with the master nearly out of gas")
		}
		return nil
	})

	got := f.flows()
	if len(got) != 1 || got[0].Kind != store.FlowGasTopUp {
		t.Fatalf("flows = %+v", got)
	}
	if got[0].Amount.Cmp(wei(10)) != 0 {
		t.Fatalf("swap amount = %s, want the configured 10", got[0].Amount)
	}

	// The cooldown is armed as the flow starts, not when it finishes, so a
	// crash mid-swap cannot produce a burst of attempts on restart.
	w := f.wallet(m.ID)
	if !w.RetryAfter.After(now) {
		t.Fatalf("cooldown not armed: %v", w.RetryAfter)
	}
	if !w.Idle() == false && w.Flow != got[0].ID {
		t.Fatalf("master not claimed by the top-up")
	}
}

func TestEvaluateGasDoesNothingWhenDisabled(t *testing.T) {
	f := newFixture(t)
	m := f.master(50)
	off := gasCfg
	off.SwapEnabled = false

	f.update(func(tx *store.Tx) error {
		started, err := EvaluateGas(tx, off, m, wei(0), now)
		if err != nil {
			return err
		}
		if started {
			t.Fatal("swapped with swapping disabled")
		}
		return nil
	})
	if got := f.flows(); len(got) != 0 {
		t.Fatalf("flows = %+v", got)
	}
}

func TestEvaluateGasRespectsTheCooldownAcrossCalls(t *testing.T) {
	// The bound that stops a losing swap from repeating until the fees are gone.
	f := newFixture(t)
	m := f.master(500)

	f.update(func(tx *store.Tx) error {
		_, err := EvaluateGas(tx, gasCfg, m, wei(0), now)
		return err
	})
	// Finish the flow, leaving the master idle but still cooling down.
	f.update(func(tx *store.Tx) error {
		return tx.EachFlow(func(fl store.Flow) error { return tx.DeleteFlow(fl) })
	})

	reloaded := f.wallet(m.ID)
	f.update(func(tx *store.Tx) error {
		started, err := EvaluateGas(tx, gasCfg, reloaded, wei(0), now.Add(time.Minute))
		if err != nil {
			return err
		}
		if started {
			t.Fatal("started a second swap inside the cooldown")
		}
		return nil
	})
	f.update(func(tx *store.Tx) error {
		started, err := EvaluateGas(tx, gasCfg, reloaded, wei(0), now.Add(2*time.Hour))
		if err != nil {
			return err
		}
		if !started {
			t.Fatal("did not resume once the cooldown expired")
		}
		return nil
	})
}
