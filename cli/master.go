package cli

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"time"

	"bsc/chain"
	"bsc/config"
	"bsc/keys"
	"bsc/usdt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// The master's gas is an operator's problem, not the service's — and since §53
// it is not a command's either.
//
// bsc used to refill itself: it collected a fee on every withdrawal and, when
// the master ran low, traded those fees back into native currency without being
// asked. The fee went first, because a payments primitive has no business
// charging one (§33); then the automatic trade, because it was the only
// operation whose outcome was a price rather than a yes or no, started on the
// service's own initiative (§38); and finally `bsc swap` itself, because the
// master is the operator's own wallet and trading from it is something they can
// do with any wallet they already trust (§49, §53).
//
// What is left is one command and one alert. `bsc check` answers "how is the
// master doing", and `bsc_master_bnb_wei` is what pages somebody when it stops
// being fine.

// masterCmds returns the operator commands that act on the master wallet.
func masterCmds(v *viper.Viper, code *int) []*cobra.Command {
	return []*cobra.Command{checkCmd(v, code)}
}

func checkCmd(v *viper.Viper, code *int) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Report the master's balances and the gas floor",
		Long: "Dials the configured endpoint and prints what an operator needs to decide\n" +
			"whether the master needs topping up: its native and token balances, the gas\n" +
			"floor it is judged against, and the current gas price.\n\n" +
			"It exits non-zero below the floor, so it works from cron or a check script,\n" +
			"and it reads no database, so it is safe to run while the service is up.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			m, err := dialMaster(ctx, v)
			if err != nil {
				*code = 1
				return err
			}
			defer m.close()
			return m.report(ctx, cmd.OutOrStdout(), code)
		},
	}
}

// masterCtx is what the command needs: a dialled chain, the resolved
// configuration, and the master key.
type masterCtx struct {
	cfg      config.Config
	rpc      *chain.Client
	key      keys.Key
	decimals uint8
	abi      *usdt.Usdt
}

func dialMaster(ctx context.Context, v *viper.Viper) (*masterCtx, error) {
	cfg, err := config.Parse(v)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	ring, err := keys.ParseHex(cfg.MasterSecret)
	if err != nil {
		return nil, err
	}
	key, err := ring.Master()
	if err != nil {
		return nil, err
	}
	rpc, err := chain.Dial(ctx, cfg.RPCURL, cfg.RPCRateLimit)
	if err != nil {
		return nil, err
	}
	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		rpc.Close()
		return nil, err
	}
	if err := cfg.ResolveChain(chainID); err != nil {
		rpc.Close()
		return nil, err
	}
	decimals, err := rpc.TokenDecimals(ctx, cfg.Token)
	if err != nil {
		rpc.Close()
		return nil, err
	}
	return &masterCtx{cfg: cfg, rpc: rpc, key: key, decimals: decimals, abi: usdt.NewUsdt()}, nil
}

func (m *masterCtx) close() { m.rpc.Close() }

func (m *masterCtx) tokenBalance(ctx context.Context) (*big.Int, error) {
	raw, err := m.rpc.Call(ctx, m.cfg.Token, m.abi.PackBalanceOf(m.key.Address))
	if err != nil {
		return nil, err
	}
	return m.abi.UnpackBalanceOf(raw)
}

// belowFloor is the decision on its own, so it can be tested without a chain.
// An unset or zero floor means there is nothing to be below, which reads as
// "fine" rather than as "always low" — getting that backwards would make a cron
// line page on every run.
func (m *masterCtx) belowFloor(native *big.Int) bool {
	if m.cfg.GasFloor == nil || m.cfg.GasFloor.Sign() <= 0 {
		return false
	}
	return native.Cmp(m.cfg.GasFloor) < 0
}

func (m *masterCtx) report(ctx context.Context, w io.Writer, code *int) error {
	native, err := m.rpc.BalanceBNB(ctx, m.key.Address)
	if err != nil {
		*code = 1
		return err
	}
	tokens, err := m.tokenBalance(ctx)
	if err != nil {
		*code = 1
		return err
	}
	gasPrice, err := m.rpc.GasPrice(ctx)
	if err != nil {
		*code = 1
		return err
	}

	fmt.Fprintf(w, "master     %s\n", m.key.Address.Hex())
	fmt.Fprintf(w, "chain      %d\n", m.cfg.ChainID)
	fmt.Fprintf(w, "token      %s (%d decimals)\n", m.cfg.Token.Hex(), m.decimals)
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "native     %s  (%s wei)\n", dec(native, 18), native)
	fmt.Fprintf(w, "tokens     %s  (%s base units)\n", dec(tokens, m.decimals), tokens)
	fmt.Fprintf(w, "gas floor  %s\n", dec(m.cfg.GasFloor, 18))
	fmt.Fprintf(w, "gas price  %s gwei\n", dec(gasPrice, 9))

	if m.belowFloor(native) {
		short := new(big.Int).Sub(m.cfg.GasFloor, native)
		fmt.Fprintf(w, "\nBELOW THE GAS FLOOR — nothing refills this. Send at least %s native to\n", dec(short, 18))
		fmt.Fprintf(w, "%s. The tokens above are yours to move with any wallet\n", m.key.Address.Hex())
		fmt.Fprintf(w, "that holds this key; bsc does not trade them (§53).\n")
		*code = 1
	}
	return nil
}
