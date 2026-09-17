// Package flow holds bsc's pipeline rules: which state a flow starts in, what
// transaction each state owes, where it goes next, and when new work should
// exist at all.
//
// Everything here is a pure function over values. There is no I/O, no store and
// no clock of its own, which is what makes the rules testable with synthetic
// events — no RPC fake, no confirmer fake, no sleeping.
//
// The division of labour is deliberate: this package decides *what* should
// happen, the store records it, and sender/ builds and signs the transaction.
// A flow's state is itself the instruction — StateFunding means "the funding
// transfer still needs to go out" — so committing a state change is enqueueing
// the work, and a crash between the two re-derives the same action.
package flow

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// ErrTerminal means a flow that has already finished was asked to advance.
var ErrTerminal = errors.New("flow: already terminal")

// Params describes the flow to begin.
type Params struct {
	Kind       store.FlowKind
	Wallet     uuid.UUID
	To         common.Address // the drain destination, or the payout recipient
	Amount     *big.Int       // exact amount; must be unset for a drain, which sweeps everything
	Withdrawal uuid.UUID      // the withdrawal this transfer pays, when it pays one
	Active     bool           // whether the wallet has already approved the master
	Now        time.Time
}

// Begin builds the initial flow record, choosing the entry state from whether
// the wallet is already active. Activation is not a separate flow — funding and
// approving are simply the prefix of whichever flow needed an inactive wallet —
// so an active wallet starts directly at the state that does the real work.
func Begin(p Params) (store.Flow, error) {
	if p.Wallet == uuid.Nil {
		return store.Flow{}, errors.New("flow: wallet must be set")
	}
	if err := validate(p); err != nil {
		return store.Flow{}, err
	}
	state := store.StateFunding
	if p.Active {
		state = mainState(p.Kind)
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	return store.Flow{
		ID:         uuid.New(),
		Kind:       p.Kind,
		State:      state,
		Wallet:     p.Wallet,
		Withdrawal: p.Withdrawal,
		Amount:     p.Amount,
		To:         p.To,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// validate enforces the one invariant a transfer has: an amount and a
// withdrawal id travel together or not at all.
//
// With them, the flow is paying that withdrawal exactly what it promised.
// Without them, it is sweeping whatever the wallet holds, read from the chain
// when the transaction is signed. Half of either is a flow that cannot say what
// it intends to move, and it is refused here rather than discovered at
// settlement (§46).
func validate(p Params) error {
	switch p.Kind {
	case store.FlowTransfer:
		if p.To == (common.Address{}) {
			return errors.New("flow: a transfer needs a destination")
		}
		hasAmount := p.Amount != nil && p.Amount.Sign() > 0
		switch {
		case p.Amount != nil && p.Amount.Sign() < 0:
			return errors.New("flow: a transfer cannot move a negative amount")
		case hasAmount && p.Withdrawal == uuid.Nil:
			return errors.New("flow: a transfer with an amount must name the withdrawal it pays")
		case !hasAmount && p.Withdrawal != uuid.Nil:
			return errors.New("flow: a transfer paying a withdrawal needs a positive amount")
		}
	case store.FlowPrewarm:
		if p.Amount != nil && p.Amount.Sign() != 0 {
			return errors.New("flow: prewarm must not carry an amount")
		}
		if p.Withdrawal != uuid.Nil {
			return errors.New("flow: prewarm pays nothing")
		}
	default:
		return fmt.Errorf("flow: unknown kind %d", p.Kind)
	}
	return nil
}

// mainState is the state that performs a kind's actual work, reached once the
// wallet is active. Prewarm has none: activating *is* its work.
func mainState(k store.FlowKind) store.FlowState {
	switch k {
	case store.FlowTransfer:
		return store.StateMoving
	case store.FlowPrewarm:
		return store.StateDone
	}
	return store.StateFailed
}

// Next returns the state a flow moves to once the transaction it was waiting on
// reaches finality. ok is false when that transaction reverted.
//
// ok is also true for an action the sender found unnecessary — a wallet that
// already holds enough gas, or whose allowance is already set. Those advance
// without a transaction, which is how on-chain idempotency checks survive into
// a model where waiting is a state.
func Next(f store.Flow, ok bool) (store.FlowState, error) {
	if f.State.IsTerminal() {
		return f.State, fmt.Errorf("%w: %s flow in %s", ErrTerminal, f.Kind, f.State)
	}
	if !ok {
		return store.StateFailed, nil
	}
	switch f.State {
	case store.StateFunding:
		return store.StateApproving, nil
	case store.StateApproving:
		return mainState(f.Kind), nil
	case store.StateMoving:
		return store.StateDone, nil
	}
	return store.StateFailed, fmt.Errorf("flow: unreachable state %s", f.State)
}

// ActionKind is the chain operation a state owes.
type ActionKind uint8

const (
	ActionNone    ActionKind = iota // terminal: nothing to send
	ActionFund                      // master sends the wallet enough BNB for its approve
	ActionApprove                   // the wallet approves the master for MaxUint256
	// ActionMove is one action for both shapes of transfer. A nil Amount means
	// "everything the wallet holds", resolved from the chain at signing; a set
	// one means exactly that. Two actions that differed only in where the
	// number came from were two names for one transferFrom (§46).
	ActionMove
)

func (k ActionKind) String() string {
	switch k {
	case ActionNone:
		return "none"
	case ActionFund:
		return "fund"
	case ActionApprove:
		return "approve"
	case ActionMove:
		return "move"
	}
	return "unknown"
}

// Action is what a flow's current state owes the chain. It describes the intent
// only: sender/ decides gas, nonce and signature, and for a sweep reads the
// balance at signing time.
type Action struct {
	Kind   ActionKind
	Wallet uuid.UUID      // the managed wallet the value moves from
	To     common.Address // destination, for Move
	Amount *big.Int       // exact amount, or nil to move everything
}

// Needed reports whether this action requires a transaction.
func (a Action) Needed() bool { return a.Kind != ActionNone }

// NextAction returns the action a flow's current state owes.
func NextAction(f store.Flow) Action {
	switch f.State {
	case store.StateFunding:
		return Action{Kind: ActionFund, Wallet: f.Wallet}
	case store.StateApproving:
		return Action{Kind: ActionApprove, Wallet: f.Wallet}
	case store.StateMoving:
		// Amount rides along as it is: nil for a sweep, exact for a payout. The
		// sender reads which from the value rather than from a kind.
		return Action{Kind: ActionMove, Wallet: f.Wallet, To: f.To, Amount: f.Amount}
	}
	return Action{Kind: ActionNone, Wallet: f.Wallet}
}
