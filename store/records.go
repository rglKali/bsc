package store

import (
	"math/big"
	"strconv"
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
	vPending    = 1
)

// A wallet record has no kind. There used to be one, separating the wallets bsc
// derives for callers from the master — and before that carrying the topology
// as well, baking a two-level shape into the type system.
//
// The topology became a field: a wallet with a DrainTo forwards, a wallet
// without one accumulates, and any wallet can be either (§32). The master
// stopped being a row at all: it is the operator's own key, its address lives
// in meta, and bsc stores nothing else about it (§49). So every record here is
// the same kind of thing — derived, and nameable by its caller.
// WalletID is a wallet's sequential number, and the message its private key is
// derived from: priv = HMAC-SHA256(master_secret, be64(id)).
//
// Sequential rather than random is what makes the money recoverable without
// this database. Under UUIDs the id was half the key material and 122 random
// bits are not searchable, so losing the file lost every address even while
// holding the secret. Now a recovery walks 0, 1, 2 … and asks the chain (§48).
//
// It is 8 bytes big-endian everywhere it appears — as the wallet's own key, as
// the leading part of every wallet-scoped composite key, and as the derivation
// message — so bbolt's byte order is numeric order, which is what lets the next
// id be read as "the last one, plus one" with no counter to keep in step.
//
// Ids start at 1; zero is not a wallet.
type WalletID uint64

// Ids start at 1. Zero is not a wallet and never becomes one — it is refused by
// both the store and keys.Derive — which keeps it usable as "unset" in the one
// place that matters: a call site that forgot to fill the field in.

// Key renders the id as its 8-byte sortable form.
func (id WalletID) Key() []byte { return be64(uint64(id)) }

func (id WalletID) String() string { return strconv.FormatUint(uint64(id), 10) }

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
	ID      WalletID
	Ref     string // the caller's handle for this wallet; unique across the service
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
	e.walletID(w.ID)
	e.str(w.Ref)
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
	w.ID = d.walletID()
	w.Ref = d.str()
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
	Wallet     WalletID
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
	e.walletID(f.Wallet)
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
	f.Wallet = d.walletID()
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
// bsc manages. It is an observation and nothing else — there is no lifecycle,
// because a deposit is only ever recorded *after* it happened (§50).
//
// Amount is the chain's own figure, unscaled and unrounded. There is no second
// unit beside it and no floor applied to it, so there is no dust — the number
// here is the number on the block explorer (§36).
type Deposit struct {
	Wallet    WalletID
	Block     uint64
	LogIndex  uint32
	TxHash    common.Hash
	From      common.Address
	Amount    *big.Int
	CreatedAt time.Time
}

// Cursor is this deposit's position in the feed.
func (dp *Deposit) Cursor() Cursor { return Cursor{Block: dp.Block, LogIndex: dp.LogIndex} }

func (dp *Deposit) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vDeposit)
	e.walletID(dp.Wallet)
	e.u64(dp.Block)
	e.u32(dp.LogIndex)
	e.hash(dp.TxHash)
	e.addr(dp.From)
	e.wei(dp.Amount)
	e.stamp(dp.CreatedAt)
	return e.b, e.err
}

func decodeDeposit(b []byte) (Deposit, error) {
	d := newDec(b)
	_ = d.u8()
	var dp Deposit
	dp.Wallet = d.walletID()
	dp.Block = d.u64()
	dp.LogIndex = d.u32()
	dp.TxHash = d.hash()
	dp.From = d.addr()
	dp.Amount = d.wei()
	dp.CreatedAt = d.stamp()
	return dp, d.done()
}

// Pending is a debit that has been accepted but has not happened yet: a promise
// bsc made to its caller and is now responsible for keeping.
//
// It lives in state/ rather than log/, and that is the whole point of splitting
// the two (§51). A pending withdrawal is not a movement — nothing has left the
// wallet — so it has no business in a namespace whose contract is "a record of
// what happened, kept forever". The bucket a record sits in *is* its status:
// there is no stored `pending`/`confirmed` field that could disagree with
// anything, because being here is what pending means.
//
// It carries what a promise needs and nothing a fact needs: no TxHash and no
// Block, because neither exists yet, and Attempts/Error, which are diagnostics
// about keeping the promise rather than anything about the movement.
//
// A withdrawal has NO FAILURE STATE (§28). What cannot be honoured is refused
// synchronously at creation and never becomes a record; what breaks afterwards
// is ours to retry, so a record stays here — and stays committed against its
// wallet — until it settles.
type Pending struct {
	ID          uuid.UUID
	Wallet      WalletID
	Reason      DebitReason
	PartOf      uuid.UUID // a fee names the payout it accompanies
	Destination common.Address
	Amount      *big.Int

	Attempts   uint32
	Error      string // the last attempt's, for an operator; never a verdict
	RetryAfter time.Time

	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Settle turns a promise into the observation that it was kept.
func (p *Pending) Settle(txHash common.Hash, block uint64, now time.Time) Withdrawal {
	return Withdrawal{
		ID: p.ID, Wallet: p.Wallet, Reason: p.Reason, PartOf: p.PartOf,
		Destination: p.Destination, Amount: p.Amount,
		TxHash: txHash, Block: block,
		IdempotencyKey: p.IdempotencyKey,
		CreatedAt:      p.CreatedAt,
		SettledAt:      now.UTC(),
	}
}

// Withdrawal is money that has left a managed wallet — a debit, and a fact.
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
// It lives in log/ and only ever arrives here settled: a promise is a Pending
// until the chain says otherwise, and settling moves the record (§51). There is
// no status field — being here is what confirmed means — and no Attempts or
// Error, because how many tries it took is a story about the promise, not about
// the movement.
type Withdrawal struct {
	ID          uuid.UUID
	Wallet      WalletID // the wallet it was paid from
	Reason      DebitReason
	Destination common.Address

	// PartOf links a fee to the payout it accompanies. The two are separate
	// records that settle independently — two transfers cannot be made atomic
	// without a contract — so this is what says they were asked for together
	// (§43).
	PartOf uuid.UUID
	Amount *big.Int
	TxHash common.Hash

	// Block is the finalized block the transfer landed in. It is the feed's
	// ordering: a caller polls settled debits the same way it polls deposits.
	Block uint64

	IdempotencyKey string
	CreatedAt      time.Time // when it was asked for
	SettledAt      time.Time // when the chain confirmed it
}

// Settled is this withdrawal's position in the finalized-debit feed.
func (wd *Withdrawal) Settled() Settled { return Settled{Block: wd.Block, ID: wd.ID} }

func (wd *Withdrawal) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vWithdrawal)
	e.id(wd.ID)
	e.walletID(wd.Wallet)
	e.u8(uint8(wd.Reason))
	e.id(wd.PartOf)
	e.addr(wd.Destination)
	e.wei(wd.Amount)
	e.hash(wd.TxHash)
	e.u64(wd.Block)
	e.str(wd.IdempotencyKey)
	e.stamp(wd.CreatedAt)
	e.stamp(wd.SettledAt)
	return e.b, e.err
}

func (p *Pending) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vPending)
	e.id(p.ID)
	e.walletID(p.Wallet)
	e.u8(uint8(p.Reason))
	e.id(p.PartOf)
	e.addr(p.Destination)
	e.wei(p.Amount)
	e.u32(p.Attempts)
	e.str(p.Error)
	e.stamp(p.RetryAfter)
	e.str(p.IdempotencyKey)
	e.stamp(p.CreatedAt)
	e.stamp(p.UpdatedAt)
	return e.b, e.err
}

func decodePending(b []byte) (Pending, error) {
	d := newDec(b)
	_ = d.u8()
	var p Pending
	p.ID = d.id()
	p.Wallet = d.walletID()
	p.Reason = DebitReason(d.u8())
	p.PartOf = d.id()
	p.Destination = d.addr()
	p.Amount = d.wei()
	p.Attempts = d.u32()
	p.Error = d.str()
	p.RetryAfter = d.stamp()
	p.IdempotencyKey = d.str()
	p.CreatedAt = d.stamp()
	p.UpdatedAt = d.stamp()
	return p, d.done()
}

func decodeWithdrawal(b []byte) (Withdrawal, error) {
	d := newDec(b)
	_ = d.u8()
	var wd Withdrawal
	wd.ID = d.id()
	wd.Wallet = d.walletID()
	wd.Reason = DebitReason(d.u8())
	wd.PartOf = d.id()
	wd.Destination = d.addr()
	wd.Amount = d.wei()
	wd.TxHash = d.hash()
	wd.Block = d.u64()
	wd.IdempotencyKey = d.str()
	wd.CreatedAt = d.stamp()
	wd.SettledAt = d.stamp()
	return wd, d.done()
}

// orZero makes a nil amount usable without nil-checking money everywhere.
func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
