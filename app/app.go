// Package app wires bsc's parts together and runs them.
//
// One process, one store, four goroutine groups: the watcher folding finalized
// blocks into the store, the sender signing and broadcasting, the HTTP API, and
// a snapshotter taking consistent backups. They never call each other — work is
// handed over as records, and the only direct coupling is a nudge from the
// watcher to the sender so it need not poll.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"bsc/api"
	"bsc/chain"
	"bsc/config"
	"bsc/keys"
	"bsc/sender"
	"bsc/store"
	"bsc/watcher"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

// Run starts everything and blocks until ctx is cancelled. The configuration is
// passed in rather than loaded here, so the command line owns how it is
// assembled and this package owns only what to do with it.
func Run(ctx context.Context, cfg config.Config) error {
	log := slog.Default()

	// The master secret is parsed before anything else opens, so a bad secret
	// fails immediately rather than at the first transfer.
	ring, err := keys.ParseHex(cfg.MasterSecret)
	if err != nil {
		return fmt.Errorf("master secret: %w", err)
	}
	master, err := ring.Master()
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck

	rpc, err := chain.Dial(ctx, cfg.RPCURL, cfg.RPCRateLimit)
	if err != nil {
		return err
	}
	defer rpc.Close()

	// The endpoint says which chain this is, and everything chain-shaped follows
	// from that: the token, the swap router, and the id every signature is bound
	// to. Asking removes the one setting that could disagree with reality (§26).
	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		return err
	}
	if err := cfg.ResolveChain(chainID); err != nil {
		return err
	}

	// The token's decimals are still read and recorded, even though no amount
	// is scaled by them any more: they are what lets an operator — and `bsc
	// check` — render a raw base-unit figure as something human. Recording them
	// also keeps the database's identity complete (§36).
	decimals, err := rpc.TokenDecimals(ctx, cfg.Token)
	if err != nil {
		return err
	}
	// Recorded once and checked on every later start. Every amount in this
	// database is denominated by this token on this chain, and every wallet in
	// it was derived for it, so pointing the service at another is a migration
	// rather than a configuration change — and now it cannot be done by accident.
	if err := st.Update(func(tx *store.Tx) error {
		return tx.SetMeta(store.Meta{ChainID: chainID, Token: cfg.Token, Decimals: decimals})
	}); err != nil {
		return err
	}

	// The master is recorded like any other wallet so its token balance is
	// tracked from the same Transfer logs as everything else — which is what
	// lets `bsc check` report it without an extra call.
	masterWallet, err := ensureMasterWallet(st, master.Address)
	if err != nil {
		return err
	}

	snd, err := sender.New(st, rpc, ring, sender.Options{
		ChainID: cfg.ChainID,
		Token:   cfg.Token,

		FundingMultiplier: cfg.FundingMultiplier,
		GasMultiplier:     cfg.GasMultiplier,
		RebroadcastAfter:  cfg.RebroadcastAfter,
	})
	if err != nil {
		return err
	}

	addrs := watcher.NewAddrSet()
	wat := watcher.New(st, rpc, addrs, watcher.Options{
		StartBlock:    cfg.StartBlock,
		Poll:          cfg.PollInterval,
		BackfillBatch: cfg.BackfillBatch,

		DrainThreshold: cfg.DrainThreshold,
		MasterWallet:   masterWallet.ID,
		Master:         master.Address,
		MasterPoll:     cfg.MasterPoll,
		Token:          cfg.Token,
		// The only direct coupling between roles: a commit may have created
		// work, so the sender is told rather than left to poll for it.
		Notify: snd.Notify,
	})

	srv := api.New(st, ring, addrs, wat, api.Options{
		MaxLagBlocks: cfg.MaxLagBlocks,
		Notify:       snd.Notify,
		UI:           cfg.UIEnabled,
		Health: api.Health{
			MasterGas: wat.MasterGas,
			Floor:     cfg.GasFloor,
		},
	})

	addrs.Add(masterWallet.Address, masterWallet.ID)

	mux := http.NewServeMux()
	srv.Routes(mux)
	mux.Handle("GET /metrics", promhttp.Handler())
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Info("bsc starting",
		"db", cfg.DBPath, "addr", cfg.HTTPAddr, "rpc", cfg.RPCURL,
		"chain", chainID, "master", master.Address.Hex(),
		"token", cfg.Token.Hex(), "decimals", decimals,
		"rate_limit", cfg.RPCRateLimit)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return wat.Run(gctx) })
	g.Go(func() error { return snd.Run(gctx) })
	g.Go(func() error { return runSnapshots(gctx, st, cfg, log) })
	g.Go(func() error {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdown)
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("bsc stopped")
	return nil
}

func addressOrZero(s string) common.Address {
	if s == "" {
		return common.Address{}
	}
	return common.HexToAddress(s)
}

// ensureMasterWallet records the master as a wallet, idempotently. Its id is
// derived from the address so a restart finds the same record rather than
// creating a second one.
func ensureMasterWallet(st *store.Store, addr common.Address) (store.Wallet, error) {
	var out store.Wallet
	err := st.Update(func(tx *store.Tx) error {
		existing, ok, err := tx.WalletByAddress(addr)
		if err != nil {
			return err
		}
		if ok {
			if existing.Kind != store.KindMaster {
				return fmt.Errorf("app: %s is already recorded as a %s wallet", addr.Hex(), existing.Kind)
			}
			out = existing
			return nil
		}
		out = store.Wallet{
			ID:        uuid.NewSHA1(uuid.NameSpaceOID, addr.Bytes()),
			Kind:      store.KindMaster,
			Address:   addr,
			Balance:   new(big.Int),
			CreatedAt: time.Now().UTC(),
		}
		return tx.PutWallet(out)
	})
	return out, err
}
