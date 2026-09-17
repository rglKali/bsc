package app

import (
	"context"
	"fmt"
	"math/big"

	"bsc/chain"
	"bsc/store"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
)

// Verify runs the offline audit against a database file.
//
// It opens read-only on purpose: the live database is exclusively locked by the
// running service, which is why inspection works on snapshots. Everything it
// checks is a relationship the store itself is responsible for — reserves
// against the work that justifies them, both directions of wallet↔flow
// ownership, and every index against its records.
func Verify(path string) (store.Report, error) {
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return store.Report{}, err
	}
	defer st.Close() //nolint:errcheck

	var rep store.Report
	err = st.View(func(tx *store.Tx) error {
		var err error
		rep, err = tx.Verify()
		return err
	})
	return rep, err
}

// VerifyOnChain runs the offline audit and then compares every wallet's
// materialised balance against the chain.
//
// This is the half of the audit that needs the network, and the reason there is
// no reconciler process. The deposit log deliberately omits sub-threshold dust
// and records no debits, so a balance cannot be recomputed from our own records
// — the only authority is balanceOf. Drift here means the watcher's view of a
// transfer diverged from what the token actually did, which is the one class of
// bug the offline checks cannot see.
//
// It is read-only in both directions: nothing is written, and a snapshot works
// as well as a live copy.
func VerifyOnChain(ctx context.Context, path, rpcURL string, rateLimit int, token common.Address) (store.Report, error) {
	st, err := store.OpenReadOnly(path)
	if err != nil {
		return store.Report{}, err
	}
	defer st.Close() //nolint:errcheck

	if token == (common.Address{}) {
		token = usdt.MainnetAddress
	}
	rpc, err := chain.Dial(ctx, rpcURL, rateLimit)
	if err != nil {
		return store.Report{}, err
	}
	defer rpc.Close()

	abi := usdt.NewUsdt()
	var rep store.Report
	err = st.View(func(tx *store.Tx) error {
		var err error
		if rep, err = tx.Verify(); err != nil {
			return err
		}
		var wallets []store.Wallet
		if err := tx.EachWallet(func(w store.Wallet) error {
			wallets = append(wallets, w)
			return nil
		}); err != nil {
			return err
		}
		for _, w := range wallets {
			out, err := rpc.Call(ctx, token, abi.PackBalanceOf(w.Address))
			if err != nil {
				return fmt.Errorf("balanceOf %s: %w", w.Address.Hex(), err)
			}
			onChain, err := abi.UnpackBalanceOf(out)
			if err != nil {
				return fmt.Errorf("decode balanceOf %s: %w", w.Address.Hex(), err)
			}
			if stored := orZero(w.Balance); stored.Cmp(onChain) != 0 {
				rep.Findings = append(rep.Findings, store.Finding{
					Kind:  "balance",
					Where: "wallet/" + w.Address.Hex(),
					Detail: fmt.Sprintf("stored %s, on-chain %s (ref %q, %s)",
						stored, onChain, w.Ref, w.Kind),
				})
			}
		}
		return nil
	})
	return rep, err
}

func orZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
