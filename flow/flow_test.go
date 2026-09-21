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

// payout is a transfer of an exact amount on behalf of a withdrawal.
func payout(active bool) Params {
	return Params{
		Kind: store.FlowTransfer, Wallet: 1, To: addr(0xD0),
		Amount: big.NewInt(10), Withdrawal: uuid.New(),
		Active: active, Now: time.Now(),
	}
}

// drain is a transfer of whatever the wallet holds, resolved at signing.
func drain(active bool) Params {
	return Params{
		Kind: store.FlowTransfer, Wallet: 1, To: addr(0xD0),
		Active: active, Now: time.Now(),
	}
}

func prewarm(active bool) Params {
	return Params{
		Kind: store.FlowPrewarm, Wallet: 1, Active: active, Now: time.Now(),
	}
}

// The point of collapsing the two kinds: a drain and a payout are the same
// state machine, so they must drive identically (§46). If these two ever
// diverge, the merge was wrong.
func TestBothShapesOfTransferDriveIdentically(t *testing.T) {
	shapes := map[string]Params{"payout": payout(false), "drain": drain(false)}
	want := []ActionKind{ActionFund, ActionApprove, ActionMove}

	for name, p := range shapes {
		t.Run(name, func(t *testing.T) {
			f, err := Begin(p)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if f.State != store.StateFunding {
				t.Fatalf("begins at %s, want funding", f.State)
			}
			got, final := drive(t, f, oks(len(want))...)
			if final != store.StateDone {
				t.Fatalf("ended at %s, want done", final)
			}
			if !sameActions(got, want) {
				t.Fatalf("actions %v, want %v", got, want)
			}
		})
	}
}

// Activation is not a separate flow: funding and approving are simply the first
// two states of whatever needed an inactive wallet.
func TestPrewarmIsActivationAndNothingElse(t *testing.T) {
	f, err := Begin(prewarm(false))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	got, final := drive(t, f, true, true)
	if final != store.StateDone {
		t.Fatalf("ended at %s, want done", final)
	}
	if !sameActions(got, []ActionKind{ActionFund, ActionApprove}) {
		t.Fatalf("actions %v, want fund then approve", got)
	}
}

func TestActiveWalletSkipsStraightToTheWork(t *testing.T) {
	for name, p := range map[string]Params{"payout": payout(true), "drain": drain(true)} {
		t.Run(name, func(t *testing.T) {
			f, err := Begin(p)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			got, final := drive(t, f, true)
			if final != store.StateDone {
				t.Fatalf("ended at %s", final)
			}
			if !sameActions(got, []ActionKind{ActionMove}) {
				t.Fatalf("actions %v, want just move", got)
			}
		})
	}
}

// The one thing that distinguishes them, and it is a value rather than a type:
// a payout carries its amount, a drain resolves one at signing.
func TestOnlyAPayoutCarriesItsAmount(t *testing.T) {
	p, err := Begin(payout(true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !p.Pays() || NextAction(p).Amount == nil {
		t.Fatalf("a payout must carry its amount: %+v", p)
	}

	d, err := Begin(drain(true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if d.Pays() || NextAction(d).Amount != nil {
		t.Fatalf("a drain must not carry an amount: %+v", d)
	}
}

func TestPrewarmOnAnActiveWalletIsAlreadyDone(t *testing.T) {
	f, err := Begin(prewarm(true))
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
	f, err := Begin(payout(true))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := NextAction(f).Kind; got != ActionMove {
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
func TestDrainEndsAtItsSweep(t *testing.T) {
	// A drain moves an app's own money between its own wallets; there is no fee
	// to take, so it must finish at the sweep.
	f, err := Begin(payout(true))
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

// A half-specified transfer is refused at Begin rather than discovered at
// settlement: the amount and the withdrawal id are what say which of the two
// things a transfer is, so one without the other cannot say what it intends to
// move (§46).
func TestTransferRefusesAHalfSpecifiedShape(t *testing.T) {
	amountOnly := drain(true)
	amountOnly.Amount = big.NewInt(5)
	if _, err := Begin(amountOnly); err == nil {
		t.Fatal("a transfer with an amount but no withdrawal was accepted")
	}

	withdrawalOnly := payout(true)
	withdrawalOnly.Amount = nil
	if _, err := Begin(withdrawalOnly); err == nil {
		t.Fatal("a transfer paying a withdrawal with no amount was accepted")
	}

	negative := payout(true)
	negative.Amount = big.NewInt(-1)
	if _, err := Begin(negative); err == nil {
		t.Fatal("a transfer of a negative amount was accepted")
	}

	noDestination := drain(true)
	noDestination.To = common.Address{}
	if _, err := Begin(noDestination); err == nil {
		t.Fatal("a transfer with no destination was accepted")
	}
}

func TestRevertIsTerminalFromEveryState(t *testing.T) {
	// A reverted transfer means the amount, the balance or the allowance was
	// wrong, and none of those improve by sending it again.
	for _, state := range []store.FlowState{
		store.StateFunding, store.StateApproving, store.StateMoving,
	} {
		f := store.Flow{Kind: store.FlowTransfer, State: state}
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
	f, err := Begin(payout(false))
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
		f := store.Flow{Kind: store.FlowTransfer, State: state}
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
		"no wallet":            {Kind: store.FlowTransfer, To: addr(1)},
		"unknown kind":         {Kind: store.FlowKind(99), Wallet: 1},
		"drain without dest":   {Kind: store.FlowTransfer, Wallet: 1},
		"withdrawal no dest":   {Kind: store.FlowTransfer, Wallet: 1, Amount: big.NewInt(1), Withdrawal: uuid.New()},
		"withdrawal no amount": {Kind: store.FlowTransfer, Wallet: 1, To: addr(1), Withdrawal: uuid.New()},
		"withdrawal zero":      {Kind: store.FlowTransfer, Wallet: 1, To: addr(1), Amount: big.NewInt(0), Withdrawal: uuid.New()},
		"withdrawal no id":     {Kind: store.FlowTransfer, Wallet: 1, To: addr(1), Amount: big.NewInt(1)},
		"prewarm with amount":  {Kind: store.FlowPrewarm, Wallet: 1, Amount: big.NewInt(1)},
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
		Kind: store.FlowTransfer, Wallet: 1, To: addr(0xEE),
		Amount: big.NewInt(77), Withdrawal: wd, Active: true, Now: now,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if f.ID == uuid.Nil {
		t.Fatal("Begin did not assign an id")
	}
	if f.Withdrawal != wd || f.To != addr(0xEE) {
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
	f, err := Begin(payout(true))
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
		{ActionMove.String(), "move"},
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
