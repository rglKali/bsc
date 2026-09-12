package flow

import (
	"errors"
	"math/big"
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

// drive walks a flow to a terminal state, recording the action each state owed.
// Every step is a value passed to a pure function: no RPC, no store, no clock.
// That is the whole point of keeping the rules here.
func drive(t *testing.T, f store.Flow, results ...bool) (actions []ActionKind, final store.FlowState) {
	t.Helper()
	for _, ok := range results {
		if f.State.IsTerminal() {
			t.Fatalf("flow terminated at %s with results left to apply", f.State)
		}
		actions = append(actions, NextAction(f).Kind)
		next, err := Next(f, ok)
		if err != nil {
			t.Fatalf("Next from %s: %v", f.State, err)
		}
		f.State = next
	}
	return actions, f.State
}

func params(kind store.FlowKind, active bool) Params {
	p := Params{
		Kind: kind, Wallet: uuid.New(), App: "df", To: addr(0xD0), Active: active,
		Now: time.Now(),
	}
	if kind == store.FlowWithdrawal {
		p.Amount = big.NewInt(10)
		p.Withdrawal = uuid.New()
	}
	return p
}

func TestInactiveWalletRunsActivationAsTheFlowsPrefix(t *testing.T) {
	// Activation is not a separate flow: funding and approving are simply the
	// first two states of whatever needed an inactive wallet.
	tests := map[store.FlowKind][]ActionKind{
		store.FlowDrain:      {ActionFund, ActionApprove, ActionSweep},
		store.FlowWithdrawal: {ActionFund, ActionApprove, ActionPay},
		store.FlowPrewarm:    {ActionFund, ActionApprove},
		store.FlowHouseSweep: {ActionFund, ActionApprove, ActionSweepHouse},
	}
	for kind, want := range tests {
		f, err := Begin(params(kind, false))
		if err != nil {
			t.Fatalf("%s: Begin: %v", kind, err)
		}
		if f.State != store.StateFunding {
			t.Fatalf("%s: begins at %s, want funding", kind, f.State)
		}
		got, final := drive(t, f, oks(len(want))...)
		if final != store.StateDone {
			t.Fatalf("%s: ended at %s, want done", kind, final)
		}
		if !sameActions(got, want) {
			t.Fatalf("%s: actions %v, want %v", kind, got, want)
		}
	}
}

func TestActiveWalletSkipsStraightToTheWork(t *testing.T) {
	tests := map[store.FlowKind][]ActionKind{
		store.FlowDrain:      {ActionSweep},
		store.FlowWithdrawal: {ActionPay},
		store.FlowHouseSweep: {ActionSweepHouse},
	}
	for kind, want := range tests {
		f, err := Begin(params(kind, true))
		if err != nil {
			t.Fatalf("%s: Begin: %v", kind, err)
		}
		got, final := drive(t, f, oks(len(want))...)
		if final != store.StateDone {
			t.Fatalf("%s: ended at %s", kind, final)
		}
		if !sameActions(got, want) {
			t.Fatalf("%s: actions %v, want %v", kind, got, want)
		}
	}
}

func TestPrewarmOnAnActiveWalletIsAlreadyDone(t *testing.T) {
	f, err := Begin(params(store.FlowPrewarm, true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if f.State != store.StateDone {
		t.Fatalf("state = %s, want done", f.State)
	}
	if NextAction(f).Needed() {
		t.Fatal("a finished prewarm still owes an action")
	}
}

// TestWithdrawalEndsAtItsPayout pins the shape the ledger bought back: one
// transfer per withdrawal. The fee is charged in the books and stays in the
// wallet, so there is no second leg to run, fail, or retry (§24).
func TestWithdrawalEndsAtItsPayout(t *testing.T) {
	f, err := Begin(params(store.FlowWithdrawal, true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := NextAction(f).Kind; got != ActionPay {
		t.Fatalf("first action = %s, want pay", got)
	}
	next, err := Next(f, true)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next != store.StateDone {
		t.Fatalf("after paying = %s, want done", next)
	}
}

// TestHouseSweepResolvesItsAmountWhenSigned: the excess over an app's ledger
// moves while the flow waits, so carrying a figure would mean acting on a
// reading taken at the wrong moment.
func TestHouseSweepResolvesItsAmountWhenSigned(t *testing.T) {
	p := params(store.FlowHouseSweep, true)
	f, err := Begin(p)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	act := NextAction(f)
	if act.Kind != ActionSweepHouse {
		t.Fatalf("action = %s, want sweep_house", act.Kind)
	}
	if act.Amount != nil {
		t.Fatalf("house sweep carries a fixed amount %v", act.Amount)
	}
	if act.To != p.To {
		t.Fatalf("sweeping to %s, want the collector %s", act.To.Hex(), p.To.Hex())
	}
	if done, err := Next(f, true); err != nil || done != store.StateDone {
		t.Fatalf("after sweeping = %s (%v), want done", done, err)
	}

	// An amount fixed up front is refused rather than quietly ignored.
	p.Amount = big.NewInt(5)
	if _, err := Begin(p); err == nil {
		t.Fatal("a house sweep carrying an amount was accepted")
	}
}

func TestDrainEndsAtItsSweep(t *testing.T) {
	// A drain moves an app's own money between its own wallets; there is no fee
	// to take, so it must finish at the sweep.
	f, err := Begin(params(store.FlowDrain, true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	next, err := Next(f, true)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next != store.StateDone {
		t.Fatalf("drain after sweeping = %s, want done", next)
	}
}

func TestDrainSweepsWhateverIsThere(t *testing.T) {
	// A drain's amount is read from the chain when it is signed, so carrying one
	// on the flow would imply a decision we deliberately defer.
	f, err := Begin(params(store.FlowDrain, true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	act := NextAction(f)
	if act.Kind != ActionSweep {
		t.Fatalf("drain action = %s, want sweep", act.Kind)
	}
	if act.Amount != nil {
		t.Fatalf("drain carries amount %v", act.Amount)
	}
	p := params(store.FlowDrain, true)
	p.Amount = big.NewInt(5)
	if _, err := Begin(p); err == nil {
		t.Fatal("a drain carrying an amount was accepted")
	}
}

func TestRevertIsTerminalFromEveryState(t *testing.T) {
	// A reverted transfer means the amount, the balance or the allowance was
	// wrong, and none of those improve by sending it again.
	for _, state := range []store.FlowState{
		store.StateFunding, store.StateApproving, store.StateSweeping, store.StatePaying,
	} {
		f := store.Flow{Kind: store.FlowWithdrawal, State: state}
		next, err := Next(f, false)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if next != store.StateFailed {
			t.Fatalf("revert in %s went to %s, want failed", state, next)
		}
	}
}

func TestSkippedActionsAdvanceLikeConfirmations(t *testing.T) {
	// The sender reports success for work it found unnecessary — a wallet that
	// already holds gas, or whose allowance is already set — which is how v1's
	// on-chain idempotency checks survive into a model where waiting is a state.
	f, err := Begin(params(store.FlowDrain, false))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	got, final := drive(t, f, true, true, true)
	if final != store.StateDone || len(got) != 3 {
		t.Fatalf("actions %v ended at %s", got, final)
	}
}

func TestAdvancingATerminalFlowIsAnError(t *testing.T) {
	for _, state := range []store.FlowState{store.StateDone, store.StateFailed} {
		f := store.Flow{Kind: store.FlowDrain, State: state}
		if _, err := Next(f, true); !errors.Is(err, ErrTerminal) {
			t.Fatalf("%s: got %v, want ErrTerminal", state, err)
		}
		if NextAction(f).Needed() {
			t.Fatalf("%s still owes an action", state)
		}
	}
}

func TestBeginValidatesPerKind(t *testing.T) {
	tests := map[string]Params{
		"no wallet":            {Kind: store.FlowDrain, To: addr(1)},
		"unknown kind":         {Kind: store.FlowKind(99), Wallet: uuid.New()},
		"drain without dest":   {Kind: store.FlowDrain, Wallet: uuid.New()},
		"withdrawal no dest":   {Kind: store.FlowWithdrawal, Wallet: uuid.New(), Amount: big.NewInt(1), Withdrawal: uuid.New()},
		"withdrawal no amount": {Kind: store.FlowWithdrawal, Wallet: uuid.New(), To: addr(1), Withdrawal: uuid.New()},
		"withdrawal zero":      {Kind: store.FlowWithdrawal, Wallet: uuid.New(), To: addr(1), Amount: big.NewInt(0), Withdrawal: uuid.New()},
		"withdrawal no id":     {Kind: store.FlowWithdrawal, Wallet: uuid.New(), To: addr(1), Amount: big.NewInt(1)},
		"prewarm with amount":  {Kind: store.FlowPrewarm, Wallet: uuid.New(), Amount: big.NewInt(1)},
	}
	for name, p := range tests {
		if _, err := Begin(p); err == nil {
			t.Fatalf("%s: Begin accepted", name)
		}
	}
}

func TestBeginStampsAndCarriesContext(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	wd := uuid.New()
	f, err := Begin(Params{
		Kind: store.FlowWithdrawal, Wallet: uuid.New(), App: "lkr:acme", To: addr(0xEE),
		Amount: big.NewInt(77), Withdrawal: wd, Active: true, Now: now,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if f.ID == uuid.Nil {
		t.Fatal("Begin did not assign an id")
	}
	if f.App != "lkr:acme" || f.Withdrawal != wd || f.To != addr(0xEE) {
		t.Fatalf("context not carried: %+v", f)
	}
	if !f.CreatedAt.Equal(now) || !f.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %v / %v, want %v", f.CreatedAt, f.UpdatedAt, now)
	}
	if f.Waiting() {
		t.Fatal("a new flow is already waiting on a transaction")
	}
}

func TestBeginDefaultsTheClock(t *testing.T) {
	f, err := Begin(params(store.FlowDrain, true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if f.CreatedAt.IsZero() {
		t.Fatal("CreatedAt left zero")
	}
}

func TestActionKindLabels(t *testing.T) {
	cases := []struct{ got, want string }{
		{ActionNone.String(), "none"},
		{ActionFund.String(), "fund"},
		{ActionApprove.String(), "approve"},
		{ActionSweep.String(), "sweep"},
		{ActionPay.String(), "pay"},
		{ActionKind(99).String(), "unknown"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("label = %q, want %q", c.got, c.want)
		}
	}
}

func oks(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

func sameActions(a, b []ActionKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
