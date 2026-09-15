// Package sender is the only part of bsc that touches private keys. It claims
// the work a flow's state says is owed, decides whether the chain actually
// needs it, signs, journals, and broadcasts.
//
// Two invariants govern everything here.
//
// **Strictly sequential.** One transaction is in flight at a time, so a fresh
// pending-nonce read is always gapless and there is no nonce manager to get
// wrong. The chain is deliberately the authority on the nonce rather than a
// locally persisted counter, because the operator holds the master secret and
// signs by hand for gas top-ups — a stored counter would silently desync the
// moment they did.
//
// **Journal before broadcast.** The signed transaction is committed to the
// store before it is sent, so a crash in between re-broadcasts the *same* bytes
// — same hash, idempotent on-chain — instead of signing a second transfer. A
// transaction that may have been broadcast is therefore never abandoned: it is
// resolved by observing the chain, never by giving up and re-signing, which
// could double-spend.
package sender

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"bsc/engine"
	"bsc/flow"
	"bsc/keys"
	"bsc/metrics"
	"bsc/money"
	"bsc/store"
	"bsc/swap"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/google/uuid"
)

// maxUint256 is the allowance every managed wallet grants the master.
var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// gasLimitHeadroom pads an estimate by 20% (÷10).
const gasLimitHeadroom = 12

// Chain is the subset of the RPC client the sender needs.
type Chain interface {
	Nonce(ctx context.Context, addr common.Address) (uint64, error)
	GasPrice(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error)
	Call(ctx context.Context, to common.Address, data []byte) ([]byte, error)
	BalanceBNB(ctx context.Context, addr common.Address) (*big.Int, error)
	SendRawTx(ctx context.Context, raw []byte) (common.Hash, error)
}

// Options configure the sender.
type Options struct {
	ChainID      uint64
	Token        common.Address
	FeeCollector common.Address // where the house's money lands

	// Scale converts the ledger's cents into the chain's wei. Required for the
	// house sweep, which is the only action whose amount depends on both units.
	Scale money.Scale

	// Gas top-up. Empty Router disables swapping entirely.
	Router            common.Address
	WrappedNative     common.Address // the router's path hop, e.g. WBNB
	SlippageBPS       uint32
	SwapDeadline      time.Duration
	FundingMultiplier float64       // BNB headroom over the estimated approve cost
	GasMultiplier     float64       // bump over the suggested gas price
	RebroadcastAfter  time.Duration // how long to wait before re-sending an unconfirmed transaction
	Tick              time.Duration // idle poll interval
}

// Sender signs and broadcasts.
type Sender struct {
	store  *store.Store
	chain  Chain
	ring   *keys.Ring
	abi    *usdt.Usdt
	signer types.Signer
	master keys.Key
	opts   Options
	log    *slog.Logger
	wake   chan struct{}

	// wrapped caches the router's own wrapped-native token, resolved once.
	wrappedOnce sync.Once
	wrapped     common.Address
	wrappedErr  error
}

// New builds a sender. It derives the master key up front so a bad secret fails
// at startup rather than at the first transfer.
func New(st *store.Store, ch Chain, ring *keys.Ring, opts Options) (*Sender, error) {
	master, err := ring.Master()
	if err != nil {
		return nil, fmt.Errorf("sender: master key: %w", err)
	}
	if opts.FundingMultiplier <= 0 {
		opts.FundingMultiplier = 1.25
	}
	if opts.GasMultiplier <= 0 {
		opts.GasMultiplier = 1.10
	}
	if opts.RebroadcastAfter <= 0 {
		opts.RebroadcastAfter = 2 * time.Minute
	}
	if opts.Tick <= 0 {
		opts.Tick = time.Second
	}
	if opts.Token == (common.Address{}) {
		opts.Token = usdt.MainnetAddress
	}
	if opts.ChainID == 0 {
		opts.ChainID = 56
	}
	if opts.SwapDeadline <= 0 {
		opts.SwapDeadline = 5 * time.Minute
	}
	if opts.SlippageBPS == 0 {
		opts.SlippageBPS = 100 // 1%
	}
	return &Sender{
		store:  st,
		chain:  ch,
		ring:   ring,
		abi:    usdt.NewUsdt(),
		signer: types.LatestSignerForChainID(new(big.Int).SetUint64(opts.ChainID)),
		master: master,
		opts:   opts,
		log:    slog.Default().With("svc", "sender"),
		wake:   make(chan struct{}, 1),
	}, nil
}

// Master is the gas-paying address, for the balance gauge and operator docs.
func (s *Sender) Master() common.Address { return s.master.Address }

// Notify nudges the sender to look for work. It never blocks, so the watcher
// can call it after every commit.
func (s *Sender) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run works until ctx is cancelled. It loops while there is work, then idles
// until nudged or the tick elapses — the tick being what drives re-broadcasts
// when nothing else is happening.
func (s *Sender) Run(ctx context.Context) error {
	t := time.NewTicker(s.opts.Tick)
	defer t.Stop()
	for {
		for {
			worked, err := s.Step(ctx)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				s.log.Error("sender step failed", "err", err)
				break
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-t.C:
		}
	}
}

// Step does at most one unit of work, reporting whether it did something. Run is
// this in a loop; it is exported so a composition can drive signing
// deterministically instead of waiting for a tick.
func (s *Sender) Step(ctx context.Context) (bool, error) {
	pending, err := s.inFlight()
	if err != nil {
		return false, err
	}
	if pending != nil {
		metrics.InFlight.Set(1)
		metrics.InFlightAge.Set(time.Since(pending.CreatedAt).Seconds())
		// Sequential by design: nothing new starts until this one is finalized.
		if time.Since(pending.CreatedAt) >= s.opts.RebroadcastAfter {
			s.rebroadcast(ctx, *pending)
		}
		return false, nil
	}
	metrics.InFlight.Set(0)
	metrics.InFlightAge.Set(0)

	f, ok, err := s.next()
	if err != nil || !ok {
		return false, err
	}
	return true, s.execute(ctx, f)
}

// inFlight returns the journalled transaction awaiting finality, if any. The
// journal is emptied by the watcher when a transaction finalizes, so a non-empty
// journal *is* "something is in flight".
func (s *Sender) inFlight() (*store.Send, error) {
	var found *store.Send
	err := s.store.View(func(tx *store.Tx) error {
		return tx.EachSend(func(sd store.Send) error {
			if found == nil || sd.CreatedAt.Before(found.CreatedAt) {
				cp := sd
				found = &cp
			}
			return nil
		})
	})
	return found, err
}

// next picks the flow to work on: a gas top-up first if one is waiting, then
// the longest-waiting of everything else.
//
// The exception for gas is not a preference, it is a precondition — the master
// pays for every other transaction, so anything queued ahead of a top-up would
// fail for want of the gas the top-up is about to buy.
func (s *Sender) next() (store.Flow, bool, error) {
	var best store.Flow
	var found bool
	err := s.store.View(func(tx *store.Tx) error {
		return tx.EachFlow(func(f store.Flow) error {
			if f.State.IsTerminal() || f.Waiting() {
				return nil
			}
			switch {
			case !found:
			case best.Kind == store.FlowGasTopUp && f.Kind != store.FlowGasTopUp:
				return nil // already holding a top-up
			case f.Kind == store.FlowGasTopUp && best.Kind != store.FlowGasTopUp:
			case f.CreatedAt.Before(best.CreatedAt):
			default:
				return nil
			}
			best, found = f, true
			return nil
		})
	})
	return best, found, err
}

// outcome is what a chain pre-check decided about an action.
type outcome int

const (
	outcomeSend outcome = iota // build and broadcast
	outcomeSkip                // already true on-chain; advance without a transaction
	outcomeFail                // cannot proceed; fail the flow
)

// execute performs the action a flow's state owes.
func (s *Sender) execute(ctx context.Context, f store.Flow) error {
	w, ok, err := s.wallet(f.Wallet)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sender: flow %s references missing wallet %s", f.ID, f.Wallet)
	}
	key, err := s.key(w)
	if err != nil {
		return err
	}

	act := flow.NextAction(f)
	var (
		res    outcome
		signed *types.Transaction
		reason string
	)
	switch act.Kind {
	case flow.ActionFund:
		res, signed, err = s.fund(ctx, key)
	case flow.ActionApprove:
		res, signed, err = s.approve(ctx, key)
	case flow.ActionSweep:
		res, signed, reason, err = s.sweep(ctx, key, act.To)
	case flow.ActionPay:
		res, signed, reason, err = s.pay(ctx, key, act.To, act.Amount, f.App)
	case flow.ActionApproveRouter:
		res, signed, err = s.approveRouter(ctx, key)
	case flow.ActionSwap:
		res, signed, reason, err = s.swapForGas(ctx, key, act.Amount)
	case flow.ActionSweepHouse:
		res, signed, reason, err = s.sweepHouse(ctx, key, act.To, f.App)
	default:
		return fmt.Errorf("sender: flow %s in %s owes no action", f.ID, f.State)
	}
	if err != nil {
		return err
	}

	switch res {
	case outcomeSkip:
		metrics.ActionsSkipped.WithLabelValues(act.Kind.String()).Inc()
		s.log.Info("action already satisfied on-chain",
			"flow", f.ID, "kind", f.Kind, "action", act.Kind, "wallet", w.Address.Hex())
		return s.advance(f, true, "")
	case outcomeFail:
		s.log.Error("action cannot proceed",
			"flow", f.ID, "kind", f.Kind, "action", act.Kind, "wallet", w.Address.Hex(), "reason", reason)
		return s.advance(f, false, reason)
	}
	return s.journalAndSend(ctx, f, key.Address, signed, act)
}

// fund gives a wallet exactly enough BNB to pay for its own approve. Every
// later movement is a transferFrom signed by the master, so a wallet is funded
// once, for one transaction, and never needs gas again.
func (s *Sender) fund(ctx context.Context, key keys.Key) (outcome, *types.Transaction, error) {
	need, err := s.approveCost(ctx)
	if err != nil {
		return outcomeFail, nil, err
	}
	have, err := s.chain.BalanceBNB(ctx, key.Address)
	if err != nil {
		return outcomeFail, nil, fmt.Errorf("sender: wallet balance: %w", err)
	}
	if have.Cmp(need) >= 0 {
		return outcomeSkip, nil, nil // already funded, e.g. by a previous attempt
	}
	signed, err := s.build(ctx, s.master, key.Address, new(big.Int).Sub(need, have), nil)
	return outcomeSend, signed, err
}

// approveCost estimates what an approve will cost, with headroom.
//
// The estimate is taken from the master because the wallet has no BNB yet to
// estimate from. That is safe here: both write a zero→non-zero allowance slot
// and so cost the same, and the funding multiplier absorbs the difference if
// that ever stops holding.
func (s *Sender) approveCost(ctx context.Context) (*big.Int, error) {
	data := s.abi.PackApprove(s.master.Address, maxUint256)
	gas, err := s.chain.EstimateGas(ctx, ethereum.CallMsg{
		From: s.master.Address, To: &s.opts.Token, Data: data,
	})
	if err != nil {
		return nil, fmt.Errorf("sender: estimate approve: %w", err)
	}
	price, err := s.gasPrice(ctx)
	if err != nil {
		return nil, err
	}
	cost := new(big.Int).Mul(price, new(big.Int).SetUint64(gas))
	return bump(cost, s.opts.FundingMultiplier), nil
}

// approve grants the master an unlimited allowance, signed by the wallet itself.
func (s *Sender) approve(ctx context.Context, key keys.Key) (outcome, *types.Transaction, error) {
	allowance, err := s.allowance(ctx, key.Address)
	if err != nil {
		return outcomeFail, nil, err
	}
	if allowance.Sign() > 0 {
		return outcomeSkip, nil, nil // already approved, perhaps by an earlier run
	}
	signed, err := s.build(ctx, key, s.opts.Token, new(big.Int), s.abi.PackApprove(s.master.Address, maxUint256))
	return outcomeSend, signed, err
}

// sweep moves a wallet's entire token balance, read from the chain at signing
// time. The deposit that triggered the drain is only a trigger; balanceOf is
// the authority, so whatever arrived in the meantime leaves with it.
func (s *Sender) sweep(ctx context.Context, key keys.Key, to common.Address) (outcome, *types.Transaction, string, error) {
	balance, err := s.balance(ctx, key.Address)
	if err != nil {
		return outcomeFail, nil, "", err
	}
	if balance.Sign() == 0 {
		return outcomeSkip, nil, "", nil // nothing to move; do not spend gas proving it
	}
	signed, err := s.transferFrom(ctx, key.Address, to, balance)
	return outcomeSend, signed, "", err
}

// pay moves an exact amount, verifying against the chain first.
//
// This is the "verify at the point of spending" check that replaces a periodic
// reconciler: one RPC call, made exactly where drift would cost money, instead
// of a background sweep that mostly confirms nothing happened.
func (s *Sender) pay(ctx context.Context, key keys.Key, to common.Address, amount *big.Int, app string) (outcome, *types.Transaction, string, error) {
	balance, err := s.balance(ctx, key.Address)
	if err != nil {
		return outcomeFail, nil, "", err
	}
	if balance.Cmp(amount) < 0 {
		metrics.InsufficientBalance.WithLabelValues(app).Inc()
		return outcomeFail, nil, fmt.Sprintf("on-chain balance %s is below the %s required", balance, amount), nil
	}
	signed, err := s.transferFrom(ctx, key.Address, to, amount)
	return outcomeSend, signed, "", err
}

// approveRouter lets the swap router spend the master's tokens. Same shape as a
// managed wallet approving the master: granted once, unlimited, and skipped if
// it is already in place.
func (s *Sender) approveRouter(ctx context.Context, key keys.Key) (outcome, *types.Transaction, error) {
	if s.opts.Router == (common.Address{}) {
		return outcomeFail, nil, errors.New("sender: no swap router configured")
	}
	allowance, err := s.allowanceTo(ctx, key.Address, s.opts.Router)
	if err != nil {
		return outcomeFail, nil, err
	}
	if allowance.Sign() > 0 {
		return outcomeSkip, nil, nil
	}
	signed, err := s.build(ctx, key, s.opts.Token, new(big.Int),
		s.abi.PackApprove(s.opts.Router, maxUint256))
	return outcomeSend, signed, err
}

// swapForGas trades an exact amount of tokens for native currency.
//
// This is the only transaction bsc sends whose outcome is a price rather than a
// yes or no, so it is bounded twice: the router is asked what the trade is worth
// and the result is floored by the configured slippage, and a deadline stops a
// transaction that sits in the mempool from executing later at a price nobody
// agreed to.
func (s *Sender) swapForGas(ctx context.Context, key keys.Key, amount *big.Int) (outcome, *types.Transaction, string, error) {
	if s.opts.Router == (common.Address{}) {
		return outcomeFail, nil, "no swap router configured", nil
	}
	wrapped, err := s.wrappedNative(ctx)
	if err != nil {
		return outcomeFail, nil, "", err
	}
	held, err := s.balance(ctx, key.Address)
	if err != nil {
		return outcomeFail, nil, "", err
	}
	if held.Cmp(amount) < 0 {
		// The fees were spent or swept elsewhere since the flow started.
		return outcomeFail, nil, fmt.Sprintf("holds %s, needs %s to swap", held, amount), nil
	}

	path := []common.Address{s.opts.Token, wrapped}
	quoted, err := s.chain.Call(ctx, s.opts.Router, swap.PackGetAmountsOut(amount, path))
	if err != nil {
		return outcomeFail, nil, "", fmt.Errorf("sender: quote swap: %w", err)
	}
	amounts, err := swap.UnpackAmounts(quoted)
	if err != nil {
		return outcomeFail, nil, "", fmt.Errorf("sender: decode quote: %w", err)
	}
	if len(amounts) != len(path) {
		return outcomeFail, nil, fmt.Sprintf("router quoted %d amounts for a %d-hop path", len(amounts), len(path)), nil
	}
	minOut, err := swap.MinOut(amounts[len(amounts)-1], s.opts.SlippageBPS)
	if err != nil {
		return outcomeFail, nil, err.Error(), nil
	}

	deadline := big.NewInt(time.Now().Add(s.opts.SwapDeadline).Unix())
	data := swap.PackSwapExactTokensForETH(amount, minOut, path, key.Address, deadline)
	signed, err := s.build(ctx, key, s.opts.Router, new(big.Int), data)
	if err != nil {
		return outcomeFail, nil, "", err
	}
	s.log.Info("swapping fees for gas",
		"amount", amount, "quoted", amounts[len(amounts)-1], "min_out", minOut,
		"slippage_bps", s.opts.SlippageBPS)
	return outcomeSend, signed, "", nil
}

// wrappedNative resolves the token the swap path ends at.
//
// The swap yields native currency — the router unwraps at the end — but a path
// is a list of ERC-20s and native currency is not one, so the path must end at
// the wrapped token. Asking the router for its own removes a setting that could
// be configured inconsistently with the router actually in use. A configured
// override wins, for a fork that names the accessor something else.
func (s *Sender) wrappedNative(ctx context.Context) (common.Address, error) {
	if s.opts.WrappedNative != (common.Address{}) {
		return s.opts.WrappedNative, nil
	}
	s.wrappedOnce.Do(func() {
		out, err := s.chain.Call(ctx, s.opts.Router, swap.PackWrappedNative())
		if err != nil {
			s.wrappedErr = fmt.Errorf("sender: ask router for its wrapped token: %w", err)
			return
		}
		addr, err := swap.UnpackAddress(out)
		if err != nil {
			s.wrappedErr = fmt.Errorf("sender: %w — set swap.wrapped_native if this router names it differently", err)
			return
		}
		s.wrapped = addr
		s.log.Info("resolved the router's wrapped native token", "router", s.opts.Router.Hex(), "wrapped", addr.Hex())
	})
	return s.wrapped, s.wrappedErr
}

// sweepHouse moves what a wallet holds beyond its app's ledger to the collector.
//
// The amount is resolved here, at signing time, and from two readings taken as
// close together as they can be: what the chain says the wallet holds, and what
// the ledger says the app is owed. This is the one operation whose amount is a
// computation over somebody else's money rather than a figure agreed in advance,
// so it is deliberately conservative — a stale reading must leave money behind
// rather than take money that was owed (§25).
//
// Anything that lands after this reading is simply swept next time, and the
// direction is what makes that safe: while this transaction is in flight the
// excess can only *rise*, never fall. A landing drain lifts custody by the full
// wei but the ledger by only the floored cents, so it adds its dust to the
// excess; a settling payout lifts it by the fee. Both leave this sweep taking
// less than it could rather than more than it should.
func (s *Sender) sweepHouse(ctx context.Context, key keys.Key, to common.Address, app string) (outcome, *types.Transaction, string, error) {
	if !s.opts.Scale.Valid() {
		return outcomeFail, nil, "no token scale configured", nil
	}
	var owed money.Cents
	if err := s.store.View(func(tx *store.Tx) error {
		a, found, err := tx.App(app)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("sender: house sweep for unknown app %q", app)
		}
		owed = a.Ledger
		return nil
	}); err != nil {
		return outcomeFail, nil, "", err
	}

	held, err := s.balance(ctx, key.Address)
	if err != nil {
		return outcomeFail, nil, "", err
	}
	excess := s.opts.Scale.Excess(held, owed)
	if excess.Sign() < 0 {
		// We hold less than we owe. Sweeping is out of the question, and this is
		// an alarm rather than a transfer that did not happen: the ledger and
		// the chain disagree about money that belongs to somebody.
		metrics.SolvencyShortfalls.Inc()
		return outcomeFail, nil, fmt.Sprintf(
			"holds %s wei but the app is owed %s cents — refusing to sweep while short", held, owed), nil
	}
	if excess.Sign() == 0 {
		return outcomeSkip, nil, "", nil // nothing over; do not spend gas proving it
	}
	s.log.Info("sweeping the house's excess",
		"app", app, "wallet", key.Address.Hex(), "held", held, "owed_cents", owed, "excess", excess)
	signed, err := s.transferFrom(ctx, key.Address, to, excess)
	return outcomeSend, signed, "", err
}

// transferFrom builds the master's move of a managed wallet's tokens.
func (s *Sender) transferFrom(ctx context.Context, from, to common.Address, amount *big.Int) (*types.Transaction, error) {
	return s.build(ctx, s.master, s.opts.Token, new(big.Int),
		s.abi.PackTransferFrom(from, to, amount))
}

// build signs a transaction with a fresh gas price and pending nonce. Because
// the caller is sequential and every send is awaited, the pending nonce is
// always gapless.
func (s *Sender) build(ctx context.Context, key keys.Key, to common.Address, value *big.Int, data []byte) (*types.Transaction, error) {
	price, err := s.gasPrice(ctx)
	if err != nil {
		return nil, err
	}
	gas, err := s.chain.EstimateGas(ctx, ethereum.CallMsg{
		From: key.Address, To: &to, Value: value, Data: data,
	})
	if err != nil {
		return nil, fmt.Errorf("sender: estimate gas: %w", err)
	}
	nonce, err := s.chain.Nonce(ctx, key.Address)
	if err != nil {
		return nil, fmt.Errorf("sender: nonce: %w", err)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: price,
		Gas:      gas * gasLimitHeadroom / 10,
		To:       &to,
		Value:    value,
		Data:     data,
	})
	signed, err := types.SignTx(tx, s.signer, key.Priv)
	if err != nil {
		return nil, fmt.Errorf("sender: sign: %w", err)
	}
	return signed, nil
}

// journalAndSend commits the signed transaction and only then broadcasts it.
// The ordering is the crash-safety guarantee: a restart between the two
// re-broadcasts these exact bytes rather than signing a second transfer.
func (s *Sender) journalAndSend(ctx context.Context, f store.Flow, signer common.Address, signed *types.Transaction, act flow.Action) error {
	raw, err := signed.MarshalBinary()
	if err != nil {
		return fmt.Errorf("sender: encode transaction: %w", err)
	}
	hash := signed.Hash()
	now := time.Now().UTC()

	if err := s.store.Update(func(tx *store.Tx) error {
		if err := tx.PutSend(store.Send{
			Flow: f.ID, Signer: signer, Nonce: signed.Nonce(),
			Hash: hash, Raw: raw, CreatedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.LinkTx(hash, store.TxRef{Flow: f.ID, Signer: signer, Nonce: signed.Nonce()}); err != nil {
			return err
		}
		_, err := tx.MutateFlow(f.ID, func(f *store.Flow) error {
			f.Tx = hash
			return nil
		})
		return err
	}); err != nil {
		return fmt.Errorf("sender: journal: %w", err)
	}

	metrics.TransactionsSent.WithLabelValues(act.Kind.String()).Inc()
	s.log.Info("broadcasting",
		"flow", f.ID, "kind", f.Kind, "action", act.Kind,
		"tx", hash.Hex(), "nonce", signed.Nonce(), "signer", signer.Hex())

	if _, err := s.chain.SendRawTx(ctx, raw); err != nil {
		// Not a loss: the transaction is journalled, so it is re-broadcast
		// until it lands. Abandoning and re-signing could double-spend.
		metrics.BroadcastErrors.Inc()
		s.log.Warn("broadcast failed; will retry the journalled transaction",
			"tx", hash.Hex(), "err", err)
	}
	return nil
}

// rebroadcast re-sends a journalled transaction that has not landed. Errors
// saying the node already has it, or that its nonce is spent, mean it is out
// there and we are simply waiting on finality.
func (s *Sender) rebroadcast(ctx context.Context, sd store.Send) {
	metrics.Rebroadcasts.Inc()
	s.log.Warn("re-broadcasting an unconfirmed transaction",
		"tx", sd.Hash.Hex(), "nonce", sd.Nonce, "age", time.Since(sd.CreatedAt).Round(time.Second))
	if _, err := s.chain.SendRawTx(ctx, sd.Raw); err != nil && !alreadyBroadcast(err) {
		metrics.BroadcastErrors.Inc()
		s.log.Warn("re-broadcast failed", "tx", sd.Hash.Hex(), "err", err)
	}
}

// alreadyBroadcast reports errors that mean the transaction is already known to
// the network or its nonce has been consumed — both of which say "stop
// worrying and wait for the receipt", never "sign another one".
func alreadyBroadcast(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, known := range []string{"already known", "already exists", "nonce too low", "known transaction"} {
		if strings.Contains(msg, known) {
			return true
		}
	}
	return false
}

// advance applies a transition that needed no transaction.
func (s *Sender) advance(f store.Flow, ok bool, reason string) error {
	now := time.Now().UTC()
	return s.store.Update(func(tx *store.Tx) error {
		if reason != "" {
			f.Error = reason
		}
		_, err := engine.Advance(tx, f, ok, now)
		return err
	})
}

// key resolves a wallet record to the key that signs for it. The master is the
// master secret used directly; everything else is an HMAC derivation of its
// record id. Getting this wrong would sign with a key that controls nothing.
func (s *Sender) key(w store.Wallet) (keys.Key, error) {
	if w.Kind == store.KindMaster {
		if w.Address != s.master.Address {
			return keys.Key{}, fmt.Errorf(
				"sender: wallet %s is marked master but is not the master address", w.Address.Hex())
		}
		return s.master, nil
	}
	k, err := s.ring.Derive(w.ID)
	if err != nil {
		return keys.Key{}, fmt.Errorf("sender: derive %s: %w", w.ID, err)
	}
	return k, nil
}

func (s *Sender) wallet(id uuid.UUID) (store.Wallet, bool, error) {
	var (
		w     store.Wallet
		found bool
	)
	err := s.store.View(func(tx *store.Tx) error {
		var err error
		w, found, err = tx.Wallet(id)
		return err
	})
	return w, found, err
}

func (s *Sender) gasPrice(ctx context.Context) (*big.Int, error) {
	price, err := s.chain.GasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("sender: gas price: %w", err)
	}
	return bump(price, s.opts.GasMultiplier), nil
}

func (s *Sender) allowance(ctx context.Context, owner common.Address) (*big.Int, error) {
	return s.allowanceTo(ctx, owner, s.master.Address)
}

func (s *Sender) allowanceTo(ctx context.Context, owner, spender common.Address) (*big.Int, error) {
	out, err := s.chain.Call(ctx, s.opts.Token, s.abi.PackAllowance(owner, spender))
	if err != nil {
		return nil, fmt.Errorf("sender: allowance: %w", err)
	}
	v, err := s.abi.UnpackAllowance(out)
	if err != nil {
		return nil, fmt.Errorf("sender: decode allowance: %w", err)
	}
	return v, nil
}

func (s *Sender) balance(ctx context.Context, owner common.Address) (*big.Int, error) {
	out, err := s.chain.Call(ctx, s.opts.Token, s.abi.PackBalanceOf(owner))
	if err != nil {
		return nil, fmt.Errorf("sender: balanceOf: %w", err)
	}
	v, err := s.abi.UnpackBalanceOf(out)
	if err != nil {
		return nil, fmt.Errorf("sender: decode balanceOf: %w", err)
	}
	return v, nil
}

// bump multiplies by a float multiplier using integer percent arithmetic, so
// gas maths never touches floating point.
func bump(v *big.Int, mul float64) *big.Int {
	pct := big.NewInt(int64(mul*100 + 0.5))
	out := new(big.Int).Mul(v, pct)
	return out.Div(out, big.NewInt(100))
}
