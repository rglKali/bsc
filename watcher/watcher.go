// Package watcher follows the finalized chain and folds each block into the
// store inside a single write transaction.
//
// That single transaction is the whole point. In v1, publishing a deposit event
// and advancing the block cursor hit two different stores, which is why the
// daemon needed an ordered two-phase commit and a dedup window to make the
// result merely equivalent to exactly-once. With one store, a block's
// confirmations, balance changes, deposit records, newly started flows and the
// cursor commit together or not at all — genuinely exactly-once, with nothing to
// reconcile afterwards.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync/atomic"
	"time"

	"bsc/flow"
	"bsc/metrics"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Chain is the subset of the RPC client the watcher needs.
type Chain interface {
	Finalized(ctx context.Context) (uint64, error)
	BlockReceipts(ctx context.Context, block uint64) ([]*types.Receipt, error)
	BalanceBNB(ctx context.Context, addr common.Address) (*big.Int, error)
	// TokenBalance is only used for the master's gauge. Every managed wallet's
	// custody comes from Transfer logs; the master is not one of ours, so its
	// balance has to be asked for (§49).
	TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error)
}

// Options configure the watcher. Zero values take sensible defaults except
// where noted.
type Options struct {
	// StartBlock is used only on a fresh database. Zero means "start at the
	// current finalized head", which is the only safe default: block 0 would
	// scan the chain from genesis.
	StartBlock uint64

	Poll           time.Duration // head poll interval once caught up
	BackfillBatch  int           // blocks per write transaction while catching up
	DrainThreshold *big.Int      // don't spend gas moving less than this

	// The master, so its two balances can be gauged. It is not a wallet in the
	// store and not in the watched-address set — bsc records nothing it sends or
	// receives (§49) — so the address is all there is to pass, and an unset one
	// is what "no master" means.
	Master     common.Address // both gauges are asked of the chain
	MasterPoll time.Duration
	Token      common.Address // the USDT contract; defaults to mainnet

	// Notify is called after each successful commit so the sender can look for
	// work without polling. Optional.
	Notify func()
}

// Watcher is the block loop.
type Watcher struct {
	store *store.Store
	chain Chain
	addrs *AddrSet
	abi   *usdt.Usdt
	log   *slog.Logger

	token common.Address
	cfg   flow.Config
	opts  Options

	cursor       uint64
	nextMasterAt time.Time

	// behind is how far the finalized head is ahead of us, published for the
	// API: balances are only current once we reach the head, so accepting a
	// withdrawal while far behind would reserve against a stale balance.
	behind atomic.Uint64

	// masterGas is the master's last observed native balance, published for
	// /healthz. It is the one quantity in the service that cannot be derived
	// from logs — a manual gas top-up is a plain value transfer and emits
	// nothing — so this poll is the only moment anything knows it.
	masterGas atomic.Pointer[big.Int]

	// started is set by Start, which is what resolves the cursor. Step refuses
	// to run before it: a zero cursor means "block 0", so an unstarted watcher
	// would quietly begin walking the chain from genesis (§52).
	started bool
}

// ErrNotStarted means Step was called before Start resolved the cursor.
var ErrNotStarted = errors.New("watcher: Step before Start; the cursor is unresolved")

// Behind reports how many finalized blocks remain unprocessed.
func (w *Watcher) Behind() uint64 { return w.behind.Load() }

// MasterGas reports the master's native balance as of the last poll, or nil if
// it has not been observed yet — which is the honest answer during the first
// few seconds and after every failed poll, and is not the same as zero.
func (w *Watcher) MasterGas() *big.Int {
	if v := w.masterGas.Load(); v != nil {
		return new(big.Int).Set(v)
	}
	return nil
}

// New builds a watcher. addrs is shared with whatever creates wallets, so newly
// derived deposit addresses are matched from the next block onwards.
func New(st *store.Store, ch Chain, addrs *AddrSet, opts Options) *Watcher {
	if opts.Poll <= 0 {
		opts.Poll = 500 * time.Millisecond
	}
	if opts.BackfillBatch <= 0 {
		opts.BackfillBatch = 100
	}
	if opts.MasterPoll <= 0 {
		opts.MasterPoll = 30 * time.Second
	}
	if opts.DrainThreshold == nil {
		opts.DrainThreshold = new(big.Int)
	}
	if opts.Token == (common.Address{}) {
		opts.Token = usdt.MainnetAddress
	}
	if opts.Notify == nil {
		opts.Notify = func() {}
	}
	return &Watcher{
		store: st,
		chain: ch,
		addrs: addrs,
		abi:   usdt.NewUsdt(),
		log:   slog.Default().With("svc", "watcher"),
		token: opts.Token,
		cfg: flow.Config{
			DrainThreshold: opts.DrainThreshold,
		},
		opts: opts,
	}
}

// Run loads state, converges anything missed while the process was down, and
// then follows the chain until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	if err := w.Start(ctx); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		caughtUp, err := w.Step(ctx)
		switch {
		case ctx.Err() != nil:
			// Shutdown is decided by our own context, never by inspecting the
			// error: an upstream RPC timeout legitimately wraps
			// context.DeadlineExceeded, and treating that as a shutdown would
			// stop the watcher on a hiccup — taking every in-flight transfer
			// with it, since finality is observed here.
			return nil
		case err != nil:
			w.log.Error("block step failed", "err", err, "cursor", w.cursor)
			if !sleep(ctx, w.opts.Poll) {
				return nil
			}
		case caughtUp:
			// Only idle when there is nothing left to fetch; while behind we go
			// straight round again, because at ~2.2 blocks/s a sleep per block
			// would mean never catching up.
			if !sleep(ctx, w.opts.Poll) {
				return nil
			}
		}
	}
}

// Start loads the watch set and resolves the starting cursor, then runs the
// rules once. That startup pass is the safety net the declarative model buys:
// the rules read current state rather than react to events, so one sweep
// converges whatever was missed while the process was down.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.store.View(w.addrs.Load); err != nil {
		return fmt.Errorf("watcher: load watch set: %w", err)
	}
	metrics.WatchedAddresses.Set(float64(w.addrs.Len()))

	var (
		cursor uint64
		known  bool
	)
	if err := w.store.View(func(tx *store.Tx) error {
		var err error
		cursor, known, err = tx.Cursor()
		return err
	}); err != nil {
		return err
	}
	if !known {
		start := w.opts.StartBlock
		if start == 0 {
			head, err := w.chain.Finalized(ctx)
			if err != nil {
				return fmt.Errorf("watcher: resolve start block: %w", err)
			}
			start = head
			w.log.Info("fresh database; starting at the finalized head", "block", start)
		}
		cursor = start
	}
	w.cursor = cursor
	w.started = true

	now := time.Now()
	var started int
	if err := w.store.Update(func(tx *store.Tx) error {
		var err error
		started, err = flow.EvaluateAll(tx, w.cfg, now)
		return err
	}); err != nil {
		return fmt.Errorf("watcher: startup evaluation: %w", err)
	}
	w.log.Info("watcher starting",
		"cursor", w.cursor, "watched", w.addrs.Len(), "resumed_work", started)
	if started > 0 {
		w.opts.Notify()
	}
	return nil
}

// Step fetches and commits one batch of blocks, reporting whether it reached
// the finalized head. Run is this in a loop; it is exported so a composition can
// drive the chain deterministically rather than waiting on wall-clock timing.
func (w *Watcher) Step(ctx context.Context) (caughtUp bool, err error) {
	// Without this, an unstarted watcher reads cursor 0 and starts a backfill
	// from genesis — on mainnet that is a hundred million blocks of history,
	// and the only symptom is a service that looks busy and never catches up.
	if !w.started {
		return false, ErrNotStarted
	}
	head, err := w.chain.Finalized(ctx)
	if err != nil {
		return false, err
	}
	metrics.FinalizedBlock.Set(float64(head))
	w.pollMaster(ctx)

	if w.cursor > head {
		w.behind.Store(0)
		metrics.BlocksBehind.Set(0)
		return true, nil
	}
	behind := head - w.cursor + 1
	w.behind.Store(behind)
	metrics.BlocksBehind.Set(float64(behind))

	n := behind
	if n > uint64(w.opts.BackfillBatch) {
		n = uint64(w.opts.BackfillBatch)
	}

	// Fetch outside the write transaction: bbolt has a single writer, and
	// holding it across n RPC round-trips would block everything else.
	batch := make([][]*types.Receipt, 0, n)
	for i := uint64(0); i < n; i++ {
		receipts, err := w.chain.BlockReceipts(ctx, w.cursor+i)
		if err != nil {
			return false, fmt.Errorf("watcher: block %d: %w", w.cursor+i, err)
		}
		batch = append(batch, receipts)
	}

	caughtUp = w.cursor+n > head
	now := time.Now()
	started := time.Now()
	if err := w.store.Update(func(tx *store.Tx) error {
		for i, receipts := range batch {
			if err := w.apply(tx, w.cursor+uint64(i), receipts, now); err != nil {
				return err
			}
		}
		// App-level work depends on requests rather than block contents, and is
		// deliberately held back until we are current: starting a withdrawal
		// from a stale balance could overdraw an app.
		if caughtUp {
			if err := w.evaluatePending(tx, now); err != nil {
				return err
			}
		}
		return tx.SetCursor(w.cursor + n)
	}); err != nil {
		return false, fmt.Errorf("watcher: commit blocks %d..%d: %w", w.cursor, w.cursor+n-1, err)
	}
	metrics.CommitDuration.Observe(time.Since(started).Seconds())
	metrics.BlocksProcessed.Add(float64(n))

	w.cursor += n
	if caughtUp {
		w.behind.Store(0)
	}
	metrics.CurrentBlock.Set(float64(w.cursor - 1))
	w.opts.Notify()
	return caughtUp, nil
}

// pollMaster refreshes the master's native balance gauge. It is the one
// quantity in the system that cannot be derived from logs and our own receipts
// — an operator's manual gas top-up is a plain value transfer, which emits
// nothing — and a dry master stops every pipeline.
func (w *Watcher) pollMaster(ctx context.Context) {
	if w.opts.Master == (common.Address{}) || time.Now().Before(w.nextMasterAt) {
		return
	}
	w.nextMasterAt = time.Now().Add(w.opts.MasterPoll)
	bal, err := w.chain.BalanceBNB(ctx, w.opts.Master)
	if err != nil {
		w.log.Warn("master balance poll failed", "err", err)
		return
	}
	f, _ := new(big.Float).SetInt(bal).Float64()
	metrics.MasterBNB.Set(f)
	w.masterGas.Store(new(big.Int).Set(bal))

	// Its token balance is asked of the chain, not accumulated. The master is
	// not a wallet in this store — it is the operator's own key, spent from
	// scripts and by hand — so there is no record to maintain and no reason for
	// the watcher to care about transfers touching it (§49).
	//
	// Nothing acts on either number. Trading tokens back into gas used to start
	// here; it is an operator command now, and these two gauges are what tell
	// the operator to run it (§38).
	held, err := w.chain.TokenBalance(ctx, w.token, w.opts.Master)
	if err != nil {
		w.log.Warn("master token balance poll failed", "err", err)
		return
	}
	h, _ := new(big.Float).SetInt(held).Float64()
	metrics.MasterUSDT.Set(h)

	w.opts.Notify()
}

// sleep waits for d, reporting false if the context ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
