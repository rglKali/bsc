// Package flow holds bsc's pipeline rules: which state a flow starts in, what
// transaction each state owes, where it goes next, and when new work should
// exist at all.
//
// Everything here is a pure function over values. There is no I/O, no store and
// no clock of its own, which is what makes the rules testable with synthetic
// events — no RPC fake, no confirmer fake, no sleeping. The v1 executor could
// not be tested this way because its pipeline *was* a function that sent a
// transaction and blocked on finality; here "waiting" is a persisted state and
// advancing it is arithmetic.
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
	Kind   store.FlowKind
	Wallet uuid.UUID
	App    string
	To     common.Address // destination: the top-level for a drain, the
	// recipient for a withdrawal, the collector for a house sweep
	Amount     *big.Int  // exact amount; must be unset for a drain, which sweeps everything
	Withdrawal uuid.UUID // set for FlowWithdrawal
	Active     bool      // whether the wallet has already approved the master
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
	if p.Kind == store.FlowGasTopUp {
		// The master pays its own gas and needs no funding; what it needs is an
		// allowance for the router, which Active records just as it does for a
		// managed wallet's allowance to the master.
		state = store.StateApprovingRouter
		if p.Active {
			state = store.StateSwapping
		}
	} else if p.Active {
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
		App:        p.App,
		Withdrawal: p.Withdrawal,
		Amount:     p.Amount,
		To:         p.To,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

func validate(p Params) error {
	switch p.Kind {
	case store.FlowDrain:
		if p.To == (common.Address{}) {
			return errors.New("flow: drain needs a destination")
		}
		// A drain moves whatever is actually there, read from the chain when the
		// transaction is signed. Carrying an amount would imply otherwise.
		if p.Amount != nil && p.Amount.Sign() != 0 {
			return errors.New("flow: drain must not carry an amount")
		}
	case store.FlowWithdrawal:
		if p.To == (common.Address{}) {
			return errors.New("flow: withdrawal needs a destination")
		}
		if p.Amount == nil || p.Amount.Sign() <= 0 {
			return errors.New("flow: withdrawal needs a positive amount")
		}
		if p.Withdrawal == uuid.Nil {
			return errors.New("flow: withdrawal needs its withdrawal id")
		}
	case store.FlowPrewarm:
		if p.Amount != nil && p.Amount.Sign() != 0 {
			return errors.New("flow: prewarm must not carry an amount")
		}
	case store.FlowGasTopUp:
		// The amount is how much of the token to trade away — fixed when the
		// flow starts, so the size of the trade can never drift.
		if p.Amount == nil || p.Amount.Sign() <= 0 {
			return errors.New("flow: gas top-up needs a positive amount to swap")
		}
	case store.FlowHouseSweep:
		if p.To == (common.Address{}) {
			return errors.New("flow: house sweep needs a collector")
		}
		// Like a drain, the amount is resolved from the chain at signing time:
		// it is the excess over the app's ledger, and both halves of that
		// subtraction move while the flow waits.
		if p.Amount != nil && p.Amount.Sign() != 0 {
			return errors.New("flow: house sweep must not carry an amount")
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
	case store.FlowDrain:
		return store.StateSweeping // the whole balance
	case store.FlowWithdrawal:
		return store.StatePaying // an exact amount
	case store.FlowPrewarm:
		return store.StateDone
	case store.FlowGasTopUp:
		return store.StateSwapping
	case store.FlowHouseSweep:
		return store.StateSweepingHouse
	}
	return store.StateFailed
}

// Next returns the state a flow moves to once the transaction it was waiting on
// reaches finality. ok is false when that transaction reverted, which is
// terminal: a reverted transfer means the amount, the balance or the allowance
// was wrong, and none of those get better by sending it again.
//
// ok is also true for an action the sender found unnecessary — a wallet that
// already holds enough gas, or whose allowance is already set. Those advance
// without a transaction, which is how v1's on-chain idempotency checks survive
// into a model where waiting is a state.
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
	case store.StateApprovingRouter:
		return store.StateSwapping, nil
	case store.StateSwapping:
		return store.StateDone, nil
	case store.StatePaying:
		// A withdrawal ends here. The fee it charged never leaves in a transfer
		// of its own — it is collected by not being credited to the app — so
		// there is nothing left for the flow to do (§24).
		return store.StateDone, nil
	case store.StateSweeping, store.StateSweepingHouse:
		return store.StateDone, nil
	}
	return store.StateFailed, fmt.Errorf("flow: unreachable state %s", f.State)
}

// ActionKind is the chain operation a state owes.
type ActionKind uint8

const (
	ActionNone          ActionKind = iota // terminal: nothing to send
	ActionFund                            // master sends the wallet enough BNB for its approve
	ActionApprove                         // the wallet approves the master for MaxUint256
	ActionSweep                           // master moves the wallet's whole token balance
	ActionPay                             // master moves an exact amount from the wallet
	ActionApproveRouter                   // the master lets the swap router spend its tokens
	ActionSwap                            // the master trades tokens for native gas
	ActionSweepHouse                      // master moves a wallet's excess over the ledger
)

func (k ActionKind) String() string {
	switch k {
	case ActionNone:
		return "none"
	case ActionFund:
		return "fund"
	case ActionApprove:
		return "approve"
	case ActionSweep:
		return "sweep"
	case ActionPay:
		return "pay"
	case ActionApproveRouter:
		return "approve_router"
	case ActionSwap:
		return "swap"
	case ActionSweepHouse:
		return "sweep_house"
	}
	return "unknown"
}

// Action is what a flow's current state owes the chain. It describes the intent
// only: sender/ decides gas, nonce and signature, and for a sweep reads the
// balance at signing time.
type Action struct {
	Kind   ActionKind
	Wallet uuid.UUID      // the managed wallet the value moves from
	To     common.Address // destination, for Sweep and Pay
	Amount *big.Int       // exact amount, for Pay only
}

// Needed reports whether this action requires a transaction.
func (a Action) Needed() bool { return a.Kind != ActionNone }

// Next returns the action a flow's current state owes.
func NextAction(f store.Flow) Action {
	switch f.State {
	case store.StateFunding:
		return Action{Kind: ActionFund, Wallet: f.Wallet}
	case store.StateApproving:
		return Action{Kind: ActionApprove, Wallet: f.Wallet}
	case store.StateSweeping:
		return Action{Kind: ActionSweep, Wallet: f.Wallet, To: f.To}
	case store.StatePaying:
		return Action{Kind: ActionPay, Wallet: f.Wallet, To: f.To, Amount: f.Amount}
	case store.StateApprovingRouter:
		return Action{Kind: ActionApproveRouter, Wallet: f.Wallet}
	case store.StateSwapping:
		return Action{Kind: ActionSwap, Wallet: f.Wallet, Amount: f.Amount}
	case store.StateSweepingHouse:
		// The amount is resolved when the transaction is signed: what the wallet
		// holds, less what its app's ledger says is owed. Both move while the
		// flow waits, and only the reading taken at signing time is safe to act
		// on (§25).
		return Action{Kind: ActionSweepHouse, Wallet: f.Wallet, To: f.To}
	}
	return Action{Kind: ActionNone, Wallet: f.Wallet}
}
