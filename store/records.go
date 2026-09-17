package store

import (
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Record versions. Every record carries one, so a field can be added by
// appending it and reading it under `if v >= N` — never by renumbering or
// reordering what is already there.
//
// Nothing has been deployed, so nothing yet reads a version other than 1. The
// byte is kept because it can only be added cheaply *before* there is data:
// afterwards, giving records a version is itself a migration.
const (
	vWallet     = 1
	vFlow       = 1
	vTxRef      = 1
	vSend       = 1
	vDeposit    = 1
	vWithdrawal = 1
)

// WalletKind separates the wallets bsc derives for callers from the one it
// derives for itself.
//
// It used to carry the topology as well — a top-level hot wallet and the
// deposit addresses that drained into it — which baked a two-level shape into
// the type system. The shape is now a field: a wallet with a DrainTo forwards,
// a wallet without one accumulates, and any wallet can be either (§32).
type WalletKind uint8

const (
	KindManaged WalletKind = 1
	// KindMaster is the gas-paying wallet itself. It is recorded so its token
	// balance is tracked like any other, and so a gas top-up can own it the way
	// every other flow owns a wallet. Its key comes from the master secret
	// directly, not from HMAC derivation.
	KindMaster WalletKind = 2
)

func (k WalletKind) String() string {
	switch k {
	case KindManaged:
		return "managed"
	case KindMaster:
		return "master"
	}
	return "unknown"
}

// FlowKind is which pipeline a flow is running.
//
// There are two, and one of them does no work: a transfer moves tokens out of a
// wallet, and a prewarm only activates one.
//
// A drain and a payout used to be separate kinds running byte-identical state
// machines, differing solely in whether the amount was carried or read from the
// chain at signing — which is a property of the flow's Amount, not a property
// worth a type. Collapsing them is the same move §42 made on the records: one
// mechanism, and the reason attached rather than baked into it (§46).
type FlowKind uint8

const (
	FlowTransfer FlowKind = 1 // move tokens out of a wallet
	FlowPrewarm  FlowKind = 2 // activate a wallet off the hot path
)

func (k FlowKind) String() string {
	switch k {
	case FlowTransfer:
		return "transfer"
	case FlowPrewarm:
		return "prewarm"
	}
	return "unknown"
}

// FlowState is where a flow has got to. Activation is not a separate flow — it
// is the funding/approving prefix of whichever flow needed an inactive wallet —
// and every kind shares one terminal vocabulary, so "prewarm reached active" is
// simply StateDone.
type FlowState uint8

// The terminal states are the highest values, which is what IsTerminal reads.
// Keep it that way when adding one.
const (
	StateFunding   FlowState = 1 // send BNB for the approve, then wait
	StateApproving FlowState = 2 // approve the master, then wait
	StateMoving    FlowState = 3 // move tokens out: an exact amount, or all of them
	StateDone      FlowState = 4
	StateFailed    FlowState = 5
)

// IsTerminal reports whether the flow is finished and its record may be
// deleted. Anything worth keeping must be copied onto the withdrawal or deposit
// record in the same transaction.
func (s FlowState) IsTerminal() bool { return s >= StateDone }

func (s FlowState) String() string {
	switch s {
	case StateFunding:
		return "funding"
	case StateApproving:
		return "approving"
	case StateMoving:
		return "moving"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	}
	return "unknown"
}

// DepositStatus is the caller-visible lifecycle of money arriving.
//
// It names where the money physically is, which is the only thing bsc can
// honestly report now that it keeps no ledger. `received` means it is sitting
// on the wallet it was sent to; `forwarded` means a drain moved it on to that
// wallet's DrainTo and it is no longer here.
//
// A deposit to a wallet with no DrainTo stays `received` forever. That is not
// an unfinished state — the money is exactly where it was meant to land, and
// there is nothing further for bsc to do with it (§34).
//
// The stored values are frozen: they are a packed field in every deposit record.
type DepositStatus uint8

const (
	DepositReceived  DepositStatus = 1
	DepositForwarded DepositStatus = 2
)

func (s DepositStatus) String() string {
	switch s {
	case DepositReceived:
		return "received"
	case DepositForwarded:
		return "forwarded"
	}
	return "unknown"
}

// DebitReason says why money left a wallet.
//
// Every outgoing movement bsc makes is the same operation — the master moving a
// managed wallet's tokens somewhere — differing only in who decided it and where
// it went. Recording them as one kind of thing with a reason attached is cheaper
// than three vocabularies, and it is what lets a caller reconcile a wallet as
// credits and debits with nothing left over (§42).
//
// The reason is metadata. It changes nothing about how the transfer is built,
// signed or retried; it exists so that whoever keeps the books can tell what a
// movement meant without bsc having to know.
type DebitReason uint8

const (
	// ReasonPayout is what a caller asked for: an amount, to an address it
	// named. It begins as a promise and counts against the wallet until it
	// settles.
	ReasonPayout DebitReason = 1
	// ReasonDrain is bsc forwarding a wallet to its DrainTo. It is recorded when
	// the transfer is observed, so it is born terminal and never counts as a
	// commitment — nobody is waiting on it and it cannot be refused.
	ReasonDrain DebitReason = 2
	// ReasonFee accompanies a payout, to the wallet that pays for gas. The
	// caller decides the number; bsc only routes it (§43).
	ReasonFee DebitReason = 3
)

func (r DebitReason) String() string {
	switch r {
	case ReasonPayout:
		return "payout"
	case ReasonDrain:
		return "drain"
	case ReasonFee:
		return "fee"
	}
	return "unknown"
}

// WithdrawalStatus is the caller-visible lifecycle of money leaving.
//
// **There is no failure state.** A request that cannot be honoured is refused
// synchronously at creation — bad address, zero amount, more than the wallet
// holds — so it never becomes a record at all. Anything that goes wrong after
// that is ours: a reverted payout backs the wallet off and the withdrawal stays
// `pending` for the rules to retry. The caller is therefore never handed a
// terminal state it has to compensate for (§28, which survives the rewrite).
type WithdrawalStatus uint8

const (
	WithdrawalPending   WithdrawalStatus = 1 // accepted, not yet on-chain
	WithdrawalConfirmed WithdrawalStatus = 2 // the transfer reached finality
)

func (s WithdrawalStatus) IsTerminal() bool { return s == WithdrawalConfirmed }

func (s WithdrawalStatus) String() string {
	switch s {
	case WithdrawalPending:
		return "pending"
	case WithdrawalConfirmed:
		return "confirmed"
	}
	return "unknown"
}

// Wallet is any derived wallet. ID is the derivation handle — the private key
// is HMAC(master, ID) — so losing these records loses the ability to address
// funds.
//
// DrainTo is the whole topology. Empty means this wallet accumulates: deposits
// stay on it and withdrawals are paid from it. Set means this wallet forwards:
// everything that lands is swept to that address once it is worth the gas, and
// no withdrawal may be paid from it. A wallet cannot do both, because the two
// behaviours contradict each other — one keeps the balance, the other empties
// it (§32).
//
// The destination need not be a wallet bsc manages. When it is, the chain of
// DrainTo references is checked for cycles at write time, since A→B→A would
// otherwise burn the master's gas as fast as blocks arrive (§35).
type Wallet struct {
	ID      uuid.UUID
	Ref     string // the caller's handle for this wallet; unique across the service
	Kind    WalletKind
	Address common.Address
	DrainTo common.Address // zero = this wallet accumulates
	Active  bool           // has approved the master for MaxUint256

	// Balance is custody in the token's base units: what this address actually
	// holds, accumulated from every Transfer the watcher observed. With the
	// ledger gone this is the only balance in the service, and it means exactly
	// what the chain means by it.
	Balance *big.Int

	// Paused blocks withdrawals from this wallet. Drains are unaffected: those
	// are the topology doing what it was configured to do, not a caller
	// spending.
	Paused bool

	Flow           uuid.UUID
	FailedAttempts uint32
	RetryAfter     time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Idle reports that no flow currently owns this wallet.
func (w *Wallet) Idle() bool { return w.Flow == uuid.Nil }

// Proxies reports that this wallet forwards what it receives.
func (w *Wallet) Proxies() bool { return w.DrainTo != (common.Address{}) }

func (w *Wallet) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vWallet)
	e.id(w.ID)
	e.str(w.Ref)
	e.u8(uint8(w.Kind))
	e.addr(w.Address)
	e.addr(w.DrainTo)
	e.boolean(w.Active)
	e.wei(w.Balance)
	e.boolean(w.Paused)
	e.id(w.Flow)
	e.u32(w.FailedAttempts)
	e.stamp(w.RetryAfter)
	e.stamp(w.CreatedAt)
	e.stamp(w.UpdatedAt)
	return e.b, e.err
}

func decodeWallet(b []byte) (Wallet, error) {
	d := newDec(b)
	_ = d.u8()
	var w Wallet
	w.ID = d.id()
	w.Ref = d.str()
	w.Kind = WalletKind(d.u8())
	w.Address = d.addr()
	w.DrainTo = d.addr()
	w.Active = d.boolean()
	w.Balance = d.wei()
	w.Paused = d.boolean()
	w.Flow = d.id()
	w.FailedAttempts = d.u32()
	w.RetryAfter = d.stamp()
	w.CreatedAt = d.stamp()
	w.UpdatedAt = d.stamp()
	return w, d.done()
}

// Flow is one pipeline instance. Its state *is* the instruction to the sender —
// StateFunding means "the funding transfer still needs to go out" — so there is
// no separate intent to persist and a crash between the state committing and
// the transaction being built re-derives the same work.
//
// Amount and Withdrawal move together and are the whole difference between the
// two things a transfer can be: set, and this flow is paying an exact amount on
// behalf of a withdrawal record; unset, and it is sweeping the wallet to its
// DrainTo. flow.Begin enforces that pairing, so settlement can read it as a
// fact rather than a guess (§46).
type Flow struct {
	ID         uuid.UUID
	Kind       FlowKind
	State      FlowState
	Wallet     uuid.UUID
	Withdrawal uuid.UUID // the withdrawal this transfer pays, when it pays one
	Amount     *big.Int  // zero on a drain: the amount is read from the chain
	To         common.Address
	Tx         common.Hash // the transaction this flow is waiting on, if any
	Attempt    uint32
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Waiting reports that a transaction is in flight for this flow.
func (f *Flow) Waiting() bool { return f.Tx != (common.Hash{}) }

// Pays reports that this flow is serving a caller's withdrawal rather than
// sweeping. It is the one bit that used to be a whole flow kind, and it is an
// invariant rather than a heuristic: Begin refuses a transfer whose amount and
// withdrawal id disagree.
func (f *Flow) Pays() bool { return f.Withdrawal != uuid.Nil }

// Label is what a flow is doing, for logs, metrics and the dashboard. It uses
// the same words a debit's reason does, so one movement reads the same whether
// you are watching it happen or reading what happened (§42, §46).
func (f *Flow) Label() string {
	if f.Kind != FlowTransfer {
		return f.Kind.String()
	}
	if f.Pays() {
		return "payout"
	}
	return "drain"
}

func (f *Flow) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vFlow)
	e.id(f.ID)
	e.u8(uint8(f.Kind))
	e.u8(uint8(f.State))
	e.id(f.Wallet)
	e.id(f.Withdrawal)
	e.wei(f.Amount)
	e.addr(f.To)
	e.hash(f.Tx)
	e.u32(f.Attempt)
	e.str(f.Error)
	e.stamp(f.CreatedAt)
	e.stamp(f.UpdatedAt)
	return e.b, e.err
}

func decodeFlow(b []byte) (Flow, error) {
	d := newDec(b)
	_ = d.u8()
	var f Flow
	f.ID = d.id()
	f.Kind = FlowKind(d.u8())
	f.State = FlowState(d.u8())
	f.Wallet = d.id()
	f.Withdrawal = d.id()
	f.Amount = d.wei()
	f.To = d.addr()
	f.Tx = d.hash()
	f.Attempt = d.u32()
	f.Error = d.str()
	f.CreatedAt = d.stamp()
	f.UpdatedAt = d.stamp()
	return f, d.done()
}

// TxRef is what state/tx maps a broadcast transaction to. It carries the
// journal coordinates as well as the flow, so confirming a receipt resolves the
// flow *and* locates its send-journal entry in a single lookup.
type TxRef struct {
	Flow   uuid.UUID
	Signer common.Address
	Nonce  uint64
}

func (r *TxRef) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vTxRef)
	e.id(r.Flow)
	e.addr(r.Signer)
	e.u64(r.Nonce)
	return e.b, e.err
}

func decodeTxRef(b []byte) (TxRef, error) {
	d := newDec(b)
	_ = d.u8()
	var r TxRef
	r.Flow = d.id()
	r.Signer = d.addr()
	r.Nonce = d.u64()
	return r, d.done()
}

// Send is the signed-transaction journal, written before the transaction is
// broadcast. On restart the raw bytes are re-broadcast unchanged — same hash,
// idempotent on-chain — rather than a second transaction being signed.
type Send struct {
	Flow      uuid.UUID
	Signer    common.Address
	Nonce     uint64
	Hash      common.Hash
	Raw       []byte
	CreatedAt time.Time
}

func (s *Send) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vSend)
	e.id(s.Flow)
	e.addr(s.Signer)
	e.u64(s.Nonce)
	e.hash(s.Hash)
	e.blob(s.Raw)
	e.stamp(s.CreatedAt)
	return e.b, e.err
}

func decodeSend(b []byte) (Send, error) {
	d := newDec(b)
	_ = d.u8()
	var s Send
	s.Flow = d.id()
	s.Signer = d.addr()
	s.Nonce = d.u64()
	s.Hash = d.hash()
	s.Raw = d.blob()
	s.CreatedAt = d.stamp()
	return s, d.done()
}

// Deposit is a recorded incoming transfer: one Transfer log that paid a wallet
// bsc manages.
//
// Amount is the chain's own figure, unscaled and unrounded. There is no second
// unit beside it and no floor applied to it, so there is no dust — the number
// here is the number on the block explorer (§36).
type Deposit struct {
	Wallet   uuid.UUID
	Block    uint64
	LogIndex uint32
	TxHash   common.Hash
	From     common.Address
	Amount   *big.Int
	Status   DepositStatus
	// SweptBy is the debit that carried this deposit onward, set when a drain
	// lands. It points at a withdrawal record rather than at a bare hash, so a
	// credit and the debit that consumed it are two ends of one link (§42).
	SweptBy   uuid.UUID
	CreatedAt time.Time
}

// Cursor is this deposit's position in the feed.
func (dp *Deposit) Cursor() Cursor { return Cursor{Block: dp.Block, LogIndex: dp.LogIndex} }

func (dp *Deposit) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vDeposit)
	e.id(dp.Wallet)
	e.u64(dp.Block)
	e.u32(dp.LogIndex)
	e.hash(dp.TxHash)
	e.addr(dp.From)
	e.wei(dp.Amount)
	e.u8(uint8(dp.Status))
	e.id(dp.SweptBy)
	e.stamp(dp.CreatedAt)
	return e.b, e.err
}

func decodeDeposit(b []byte) (Deposit, error) {
	d := newDec(b)
	_ = d.u8()
	var dp Deposit
	dp.Wallet = d.id()
	dp.Block = d.u64()
	dp.LogIndex = d.u32()
	dp.TxHash = d.hash()
	dp.From = d.addr()
	dp.Amount = d.wei()
	dp.Status = DepositStatus(d.u8())
	dp.SweptBy = d.id()
	dp.CreatedAt = d.stamp()
	return dp, d.done()
}

// Withdrawal is money leaving a managed wallet — a debit.
//
// Every outgoing movement is this record: a payout the caller asked for, the fee
// that accompanied it, and a drain bsc decided to make. Reason says which. That
// is the whole of the "drain" concept now — it is a debit with a reason, linked
// to the credits it carried (§42) — and the whole of the fee (§43).
//
// There is no fee on it. A fee is a charge somebody levies on somebody else,
// which needs to know who they are, and bsc does not. A caller that wants to
// take a dollar asks for a payout a dollar smaller and moves the remainder with
// a second withdrawal of its own (§33).
type Withdrawal struct {
	ID          uuid.UUID
	Wallet      uuid.UUID // the wallet it is paid from
	Reason      DebitReason
	Destination common.Address

	// PartOf links a fee to the payout it accompanies. The two are separate
	// records that settle independently — two transfers cannot be made atomic
	// without a contract — so this is what says they were asked for together
	// (§43).
	PartOf uuid.UUID
	Amount *big.Int
	Status WithdrawalStatus
	TxHash common.Hash

	// Block is the finalized block the transfer landed in, set when the
	// withdrawal confirms. It is the feed's ordering: a caller polls settled
	// debits the same way it polls deposits.
	Block uint64
	// Error and Attempts are diagnostics, never a verdict: a withdrawal has no
	// failure state, so these describe a request still being retried (§28).
	Error          string
	Attempts       uint32
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Settled is this withdrawal's position in the finalized-debit feed. It is only
// meaningful once the withdrawal is terminal.
func (wd *Withdrawal) Settled() Settled { return Settled{Block: wd.Block, ID: wd.ID} }

func (wd *Withdrawal) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vWithdrawal)
	e.id(wd.ID)
	e.id(wd.Wallet)
	e.u8(uint8(wd.Reason))
	e.id(wd.PartOf)
	e.addr(wd.Destination)
	e.wei(wd.Amount)
	e.u8(uint8(wd.Status))
	e.hash(wd.TxHash)
	e.u64(wd.Block)
	e.str(wd.Error)
	e.u32(wd.Attempts)
	e.str(wd.IdempotencyKey)
	e.stamp(wd.CreatedAt)
	e.stamp(wd.UpdatedAt)
	return e.b, e.err
}

func decodeWithdrawal(b []byte) (Withdrawal, error) {
	d := newDec(b)
	_ = d.u8()
	var wd Withdrawal
	wd.ID = d.id()
	wd.Wallet = d.id()
	wd.Reason = DebitReason(d.u8())
	wd.PartOf = d.id()
	wd.Destination = d.addr()
	wd.Amount = d.wei()
	wd.Status = WithdrawalStatus(d.u8())
	wd.TxHash = d.hash()
	wd.Block = d.u64()
	wd.Error = d.str()
	wd.Attempts = d.u32()
	wd.IdempotencyKey = d.str()
	wd.CreatedAt = d.stamp()
	wd.UpdatedAt = d.stamp()
	return wd, d.done()
}

// orZero makes a nil amount usable without nil-checking money everywhere.
func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
