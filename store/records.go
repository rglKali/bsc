package store

import (
	"math/big"
	"time"

	"bsc/money"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Record versions. Bump one only when appending a field, and read the new field
// under `if v >= N` in that record's decoder — never renumber or reorder.
const (
	vApp        = 1
	vWallet     = 1
	vFlow       = 1
	vTxRef      = 1
	vSend       = 1
	vDeposit    = 1
	vWithdrawal = 1
)

// WalletKind separates an app's single hot wallet from the deposit addresses
// that drain into it. The distinction is load-bearing, not cosmetic: the drain
// invariant applies only to deposit wallets, since an unscoped rule would try
// to drain a top-level wallet to itself forever (§4).
type WalletKind uint8

const (
	KindTopLevel WalletKind = 1
	KindDeposit  WalletKind = 2
	// KindMaster is the gas-paying wallet itself. It is recorded so its token
	// balance — the fees it collects — is tracked like any other, and so a gas
	// top-up can own it the way every other flow owns a wallet. Its key comes
	// from the master secret directly, not from HMAC derivation.
	KindMaster WalletKind = 3
)

func (k WalletKind) String() string {
	switch k {
	case KindTopLevel:
		return "top_level"
	case KindDeposit:
		return "deposit"
	case KindMaster:
		return "master"
	}
	return "unknown"
}

// FlowKind is which pipeline a flow is running.
type FlowKind uint8

const (
	FlowDrain      FlowKind = 1 // deposit wallet -> its app's top-level
	FlowWithdrawal FlowKind = 2 // top-level -> a destination
	FlowPrewarm    FlowKind = 4 // activate a wallet off the hot path
	FlowGasTopUp   FlowKind = 5 // swap collected fees back into gas
	// FlowHouseSweep moves what a wallet holds beyond its app's ledger — fees,
	// sub-cent dust, and anything else that arrived without being owed to
	// anybody — to the collector. It replaces the fee sweep: with the ledger as
	// the authority there is no separate fee to chase, only an excess (§25).
	FlowHouseSweep FlowKind = 6
	// 3 was FlowFeeSweep, twice: first as its own flow, then as a state of the
	// withdrawal that earned it. Not reused.
)

func (k FlowKind) String() string {
	switch k {
	case FlowDrain:
		return "drain"
	case FlowWithdrawal:
		return "withdrawal"
	case FlowPrewarm:
		return "prewarm"
	case FlowGasTopUp:
		return "gas_topup"
	case FlowHouseSweep:
		return "house_sweep"
	}
	return "unknown"
}

// FlowState is where a flow has got to. Activation is not a separate flow — it
// is the funding/approving prefix of whichever flow needed an inactive wallet —
// and every kind shares one terminal vocabulary, so "prewarm reached active" is
// simply StateDone.
//
// A withdrawal ends at its payout. It used to carry on into a fee-collection
// state, which the ledger made unnecessary: the fee is collected by not
// crediting it, so a withdrawal is one transfer again (§24).
type FlowState uint8

const (
	StateFunding         FlowState = 1 // send BNB for the approve, then wait
	StateApproving       FlowState = 2 // approve the master, then wait
	StateSweeping        FlowState = 3 // move the wallet's whole balance
	StatePaying          FlowState = 4 // move an exact amount to a destination
	StateApprovingRouter FlowState = 6 // let the swap router spend the master's tokens
	StateSwapping        FlowState = 7 // trade tokens for native gas
	StateSweepingHouse   FlowState = 8 // move the wallet's excess over the ledger
	StateDone            FlowState = 10
	// 5 was StateCollecting, when a withdrawal collected its own fee.
	StateFailed FlowState = 11
)

// IsTerminal reports whether the flow is finished and its record may be
// deleted. Anything worth keeping must be copied onto the withdrawal or deposit
// record in the same transaction (§3).
func (s FlowState) IsTerminal() bool { return s >= StateDone }

func (s FlowState) String() string {
	switch s {
	case StateFunding:
		return "funding"
	case StateApproving:
		return "approving"
	case StateSweeping:
		return "sweeping"
	case StatePaying:
		return "paying"
	case StateApprovingRouter:
		return "approving_router"
	case StateSwapping:
		return "swapping"
	case StateSweepingHouse:
		return "sweeping_house"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	}
	return "unknown"
}

// DepositStatus tracks whether a detected deposit has been swept into its app's
// top-level wallet yet. Crediting is a status rather than a feed entry because
// one sweep can credit several deposits at once (§9).
type DepositStatus uint8

const (
	DepositConfirmed DepositStatus = 1 // seen in a finalized block, not yet swept
	DepositCredited  DepositStatus = 2 // its drain landed; the funds are withdrawable
)

func (s DepositStatus) String() string {
	switch s {
	case DepositConfirmed:
		return "confirmed"
	case DepositCredited:
		return "credited"
	}
	return "unknown"
}

// WithdrawalStatus is the app-visible lifecycle. There is no `partial`: fees
// accrue off-chain and sweep in batch, so a withdrawal is exactly one transfer
// and cannot half-succeed (§7).
type WithdrawalStatus uint8

const (
	WithdrawalQueued  WithdrawalStatus = 1 // accepted and reserved, not yet signed
	WithdrawalPending WithdrawalStatus = 2 // broadcast, awaiting finality
	WithdrawalDone    WithdrawalStatus = 3
	WithdrawalFailed  WithdrawalStatus = 4
)

func (s WithdrawalStatus) IsTerminal() bool { return s >= WithdrawalDone }

func (s WithdrawalStatus) String() string {
	switch s {
	case WithdrawalQueued:
		return "queued"
	case WithdrawalPending:
		return "pending"
	case WithdrawalDone:
		return "done"
	case WithdrawalFailed:
		return "failed"
	}
	return "unknown"
}

// FeePolicy is an app's withdrawal-fee configuration: a business fee charged to
// the app's own users, unrelated to gas (§7). v2.0 implements Flat and Min;
// BPS and MaxFee are carried so a percentage fee later needs no migration, and
// are rejected while non-zero.
//
// Every field is in cents. A fee that was not a whole number of cents would put
// a fraction into the ledger and cost it the exactness the whole design rests
// on, so the unit itself makes that impossible.
type FeePolicy struct {
	Flat   money.Cents // flat charge per withdrawal
	Min    money.Cents // reject withdrawals below this; also the brake on spam
	BPS    uint32      // reserved: basis points of `amount`
	MaxFee money.Cents // reserved: cap on a percentage fee
}

// Zero reports a policy that charges nothing.
func (p FeePolicy) Zero() bool { return p.Flat == 0 && p.BPS == 0 }

func (p *FeePolicy) encode(e *enc) {
	e.i64(int64(p.Flat))
	e.i64(int64(p.Min))
	e.u32(p.BPS)
	e.i64(int64(p.MaxFee))
}

func (p *FeePolicy) decode(d *dec) {
	p.Flat = money.Cents(d.i64())
	p.Min = money.Cents(d.i64())
	p.BPS = d.u32()
	p.MaxFee = money.Cents(d.i64())
}

// App is a caller. Registration is public and idempotent; apps configure
// themselves; a slug identifies them and there are no API keys (§6).
type App struct {
	Slug   string
	Wallet uuid.UUID // its top-level hot wallet
	Fee    FeePolicy

	// Ledger is what this app is owed, in cents, and it is the only authority
	// on what the app may spend. It is credited when a deposit's drain lands —
	// not when the deposit is seen — because money still sitting in a deposit
	// address cannot be paid out of the hot wallet (§22).
	//
	// It is materialised for O(1) reads but fully recomputable from log/:
	// credited deposits less settled withdrawals. That is the difference from
	// the balance it replaces, which no amount of log-reading could reproduce.
	Ledger money.Cents

	// Reserved is held by open withdrawals — payout plus fee, one figure now
	// that the fee no longer leaves separately. Spendable is Ledger-Reserved.
	Reserved money.Cents

	Paused    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Spendable is what the app may commit to a new withdrawal.
func (a *App) Spendable() money.Cents {
	if a.Reserved > a.Ledger {
		return 0
	}
	return a.Ledger - a.Reserved
}

func (a *App) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vApp)
	e.str(a.Slug)
	e.id(a.Wallet)
	a.Fee.encode(e)
	e.i64(int64(a.Ledger))
	e.i64(int64(a.Reserved))
	e.boolean(a.Paused)
	e.stamp(a.CreatedAt)
	e.stamp(a.UpdatedAt)
	return e.b, e.err
}

func decodeApp(b []byte) (App, error) {
	d := newDec(b)
	_ = d.u8()
	var a App
	a.Slug = d.str()
	a.Wallet = d.id()
	a.Fee.decode(d)
	a.Ledger = money.Cents(d.i64())
	a.Reserved = money.Cents(d.i64())
	a.Paused = d.boolean()
	a.CreatedAt = d.stamp()
	a.UpdatedAt = d.stamp()
	return a, d.done()
}

// Wallet is any derived wallet: an app's top-level or one of its deposit
// addresses. ID is the derivation handle — the private key is HMAC(master, ID)
// — so losing these records loses the ability to address funds.
//
// Flow is the one live flow that owns this wallet, or the zero UUID when idle.
// Holding a pointer rather than duplicating the flow's state keeps exactly one
// place a state can be wrong, and makes "is this wallet busy?" a field read (§4).
type Wallet struct {
	ID      uuid.UUID
	App     string
	Kind    WalletKind
	Ref     string // app-supplied handle; deposit wallets only
	Address common.Address
	Active  bool // has approved the master for MaxUint256

	// Balance is custody: the wei this address actually holds, as observed from
	// the chain. It is deliberately *not* what the app may spend — that is the
	// app's ledger, in cents. One wallet holds an app's money, the house's fees
	// and sub-cent dust in a single number, and no amount of arithmetic on it
	// can say whose is whose (§22).
	Balance *big.Int

	Flow           uuid.UUID
	FailedAttempts uint32
	RetryAfter     time.Time
	CreatedAt      time.Time
}

// Idle reports that no flow currently owns this wallet.
func (w *Wallet) Idle() bool { return w.Flow == uuid.Nil }

func (w *Wallet) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vWallet)
	e.id(w.ID)
	e.str(w.App)
	e.u8(uint8(w.Kind))
	e.str(w.Ref)
	e.addr(w.Address)
	e.boolean(w.Active)
	e.wei(w.Balance)
	e.id(w.Flow)
	e.u32(w.FailedAttempts)
	e.stamp(w.RetryAfter)
	e.stamp(w.CreatedAt)
	return e.b, e.err
}

func decodeWallet(b []byte) (Wallet, error) {
	d := newDec(b)
	_ = d.u8()
	var w Wallet
	w.ID = d.id()
	w.App = d.str()
	w.Kind = WalletKind(d.u8())
	w.Ref = d.str()
	w.Address = d.addr()
	w.Active = d.boolean()
	w.Balance = d.wei()
	w.Flow = d.id()
	w.FailedAttempts = d.u32()
	w.RetryAfter = d.stamp()
	w.CreatedAt = d.stamp()
	return w, d.done()
}

// Flow is one pipeline instance. Its state *is* the instruction to the sender —
// StateFunding means "the funding transfer still needs to go out" — so there is
// no separate intent to persist and a crash between the state committing and
// the transaction being built re-derives the same work (§4).
type Flow struct {
	ID         uuid.UUID
	Kind       FlowKind
	State      FlowState
	Wallet     uuid.UUID
	App        string
	Withdrawal uuid.UUID // set for FlowWithdrawal
	Amount     *big.Int  // zero on a sweep: the amount is read from the chain
	To         common.Address
	Tx         common.Hash // the transaction this flow is waiting on, if any
	Attempt    uint32
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Waiting reports that a transaction is in flight for this flow.
func (f *Flow) Waiting() bool { return f.Tx != (common.Hash{}) }

func (f *Flow) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vFlow)
	e.id(f.ID)
	e.u8(uint8(f.Kind))
	e.u8(uint8(f.State))
	e.id(f.Wallet)
	e.str(f.App)
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
	f.App = d.str()
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
// idempotent on-chain — rather than a second transaction being signed (§4).
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

// Deposit is a recorded incoming transfer, and one ledger entry: Cents is the
// credit the app receives when this deposit's drain lands.
//
// Both units are kept. AmountWei is what the chain says arrived and is what an
// operator compares against a block explorer; Cents is what the app was
// actually credited, floored. The difference is house dust, and storing both
// makes "the explorer says 10.007 and your API says 1000" a lookup rather than
// an investigation (§23).
//
// Transfers too small to be worth a whole cent get no record at all: there is
// no ledger entry to write, and an entry of zero would be a feed item that
// changed nothing.
type Deposit struct {
	Wallet    uuid.UUID
	App       string
	Block     uint64
	LogIndex  uint32
	TxHash    common.Hash
	From      common.Address
	AmountWei *big.Int    // what arrived on-chain
	Cents     money.Cents // what the app is credited, floored
	Status    DepositStatus
	DrainTx   common.Hash // the sweep that credited it
	CreatedAt time.Time
}

// Cursor is this deposit's position in the app's feed.
func (dp *Deposit) Cursor() Cursor { return Cursor{Block: dp.Block, LogIndex: dp.LogIndex} }

func (dp *Deposit) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vDeposit)
	e.id(dp.Wallet)
	e.str(dp.App)
	e.u64(dp.Block)
	e.u32(dp.LogIndex)
	e.hash(dp.TxHash)
	e.addr(dp.From)
	e.wei(dp.AmountWei)
	e.i64(int64(dp.Cents))
	e.u8(uint8(dp.Status))
	e.hash(dp.DrainTx)
	e.stamp(dp.CreatedAt)
	return e.b, e.err
}

func decodeDeposit(b []byte) (Deposit, error) {
	d := newDec(b)
	_ = d.u8()
	var dp Deposit
	dp.Wallet = d.id()
	dp.App = d.str()
	dp.Block = d.u64()
	dp.LogIndex = d.u32()
	dp.TxHash = d.hash()
	dp.From = d.addr()
	dp.AmountWei = d.wei()
	dp.Cents = money.Cents(d.i64())
	dp.Status = DepositStatus(d.u8())
	dp.DrainTx = d.hash()
	dp.CreatedAt = d.stamp()
	return dp, d.done()
}

// Withdrawal is an app-initiated payout, and one ledger entry: Debit is what
// leaves the app's ledger when it settles.
//
// There is a single reservation now, not two. The fee no longer leaves in a
// transfer of its own — it is collected by simply not crediting it to the app —
// so payout and fee have the same lifetime and are held as one figure, released
// together when the payout confirms or fails (§24).
//
// Fee and FeeSnapshot together keep history explicable once an app edits its
// policy, and storing the reserved figure rather than recomputing it makes the
// release exact across a policy change.
type Withdrawal struct {
	ID             uuid.UUID
	App            string
	Destination    common.Address
	Amount         money.Cents // what the app asked for
	Fee            money.Cents // charged, per the policy at request time
	Payout         money.Cents // what the destination receives, on-chain
	Debit          money.Cents // payout + fee: what this record reserves and then spends
	DeductFee      bool
	Status         WithdrawalStatus
	TxHash         common.Hash
	Error          string
	IdempotencyKey string
	FeeSnapshot    FeePolicy
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (wd *Withdrawal) encode() ([]byte, error) {
	e := &enc{}
	e.u8(vWithdrawal)
	e.id(wd.ID)
	e.str(wd.App)
	e.addr(wd.Destination)
	e.i64(int64(wd.Amount))
	e.i64(int64(wd.Fee))
	e.i64(int64(wd.Payout))
	e.i64(int64(wd.Debit))
	e.boolean(wd.DeductFee)
	e.u8(uint8(wd.Status))
	e.hash(wd.TxHash)
	e.str(wd.Error)
	e.str(wd.IdempotencyKey)
	wd.FeeSnapshot.encode(e)
	e.stamp(wd.CreatedAt)
	e.stamp(wd.UpdatedAt)
	return e.b, e.err
}

func decodeWithdrawal(b []byte) (Withdrawal, error) {
	d := newDec(b)
	_ = d.u8()
	var wd Withdrawal
	wd.ID = d.id()
	wd.App = d.str()
	wd.Destination = d.addr()
	wd.Amount = money.Cents(d.i64())
	wd.Fee = money.Cents(d.i64())
	wd.Payout = money.Cents(d.i64())
	wd.Debit = money.Cents(d.i64())
	wd.DeductFee = d.boolean()
	wd.Status = WithdrawalStatus(d.u8())
	wd.TxHash = d.hash()
	wd.Error = d.str()
	wd.IdempotencyKey = d.str()
	wd.FeeSnapshot.decode(d)
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

// FeePolicy is an app's
