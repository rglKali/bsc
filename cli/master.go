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
	"bsc/swap"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// The master's gas is an operator's problem now, not the service's.
//
// bsc used to refill itself: it collected a fee on every withdrawal and, when
// the master ran low, traded those fees back into native currency without being
// asked. Both halves are gone — the fee because a payments primitive has no
// business charging one (§33), and the automatic trade because it was the only
// operation whose outcome was a price rather than a yes or no, started on the
// service's own initiative, at whatever moment the balance happened to cross a
// line (§38).
//
// What replaces it is two commands and an alert. `bsc check` answers "how is
// the master doing", the `bsc_master_bnb_wei` gauge is what pages somebody when
// it stops being fine, and `bsc swap` is the deliberate, attended act of fixing
// it — at a price the operator has just looked at.

// masterCmds returns the operator commands that act on the master wallet.
func masterCmds(v *viper.Viper, code *int) []*cobra.Command {
	return []*cobra.Command{checkCmd(v, code), swapCmd(v, code)}
}

func checkCmd(v *viper.Viper, code *int) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Report the master's balances, the gas floor, and the current price",
		Long: "Dials the configured endpoint and prints what an operator needs to decide\n" +
			"whether to run `bsc swap`: the master's native and token balances, the gas\n" +
			"floor it is judged against, the current gas price, and what the router\n" +
			"would pay right now in both directions.\n\n" +
			"It reads no database, so it is safe to run while the service is up.",
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

func swapCmd(v *viper.Viper, code *int) *cobra.Command {
	var (
		sellUSDT string
		buyBNB   string
		ifBelow  bool
		dryRun   bool
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "swap",
		Short: "Trade the master's tokens for gas, by hand",
		Long: "Sells USDT for native currency through the configured Uniswap-V2-style\n" +
			"router, signing with the master secret.\n\n" +
			"  bsc swap --sell-usdt 25     spend exactly 25 USDT, receive whatever it buys\n" +
			"  bsc swap --buy-bnb 0.1      receive exactly 0.1 BNB, spend whatever it costs\n\n" +
			"The two are the same trade named from either end, and which one you want\n" +
			"depends on the question you are answering. `--sell-usdt` is for \"put this\n" +
			"much of the float to work\"; `--buy-bnb` is for \"the master needs a tenth of\n" +
			"a BNB\", which is the usual one, because gas is the requirement and the cost\n" +
			"is the answer.\n\n" +
			"Amounts are decimal, in whole units — this is a command a human types, not\n" +
			"the wire. The quote is printed and confirmed before anything is signed, and\n" +
			"a slippage bound is applied in whichever direction protects you: a floor on\n" +
			"what you receive, or a ceiling on what you spend.\n\n" +
			"  bsc swap --buy-bnb 0.1 --if-below --yes\n\n" +
			"...does nothing unless the master is under gas.floor_wei, which makes it\n" +
			"safe to run from cron. That is where this belongs if you want it automated:\n" +
			"the service never trades on its own initiative, and a crontab line is\n" +
			"something you can read, audit and switch off (§44).\n\n" +
			"IMPORTANT: this signs with the same key the running service signs with, and\n" +
			"nonces are read from the chain rather than counted (§7). If the service has\n" +
			"a transaction in flight, both may pick the same nonce and one will be\n" +
			"rejected — harmlessly, but you will have to re-run. Prefer a quiet moment,\n" +
			"or stop the service first.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (sellUSDT == "") == (buyBNB == "") {
				return fmt.Errorf("give exactly one of --sell-usdt or --buy-bnb")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			m, err := dialMaster(ctx, v)
			if err != nil {
				*code = 1
				return err
			}
			defer m.close()
			return m.swap(ctx, cmd, sellUSDT, buyBNB, ifBelow, dryRun, yes, code)
		},
	}
	f := cmd.Flags()
	f.StringVar(&sellUSDT, "sell-usdt", "", "tokens to spend, decimal (exact input, e.g. 25 or 25.5)")
	f.StringVar(&buyBNB, "buy-bnb", "", "native currency to receive, decimal (exact output, e.g. 0.1)")
	f.BoolVar(&ifBelow, "if-below", false, "do nothing unless the master is under gas.floor_wei (for cron)")
	f.BoolVar(&dryRun, "dry-run", false, "print the quote and stop without signing")
	f.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// masterCtx is everything both commands need: a dialled chain, the resolved
// configuration, and the master key.
type masterCtx struct {
	cfg      config.Config
	rpc      *chain.Client
	key      keys.Key
	decimals uint8
	wrapped  common.Address
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

// router resolves the wrapped-native token by asking the router itself, which
// removes a setting that could be configured inconsistently with it.
func (m *masterCtx) router(ctx context.Context) (common.Address, common.Address, error) {
	if m.cfg.SwapRouter == "" {
		return common.Address{}, common.Address{}, fmt.Errorf(
			"no swap.router is configured and none is known for chain %d", m.cfg.ChainID)
	}
	router := common.HexToAddress(m.cfg.SwapRouter)
	if m.wrapped != (common.Address{}) {
		return router, m.wrapped, nil
	}
	if m.cfg.SwapNative != "" {
		m.wrapped = common.HexToAddress(m.cfg.SwapNative)
		return router, m.wrapped, nil
	}
	raw, err := m.rpc.Call(ctx, router, swap.PackWrappedNative())
	if err != nil {
		return router, common.Address{}, fmt.Errorf("asking the router for its wrapped token: %w", err)
	}
	m.wrapped, err = swap.UnpackAddress(raw)
	return router, m.wrapped, err
}

func (m *masterCtx) tokenBalance(ctx context.Context) (*big.Int, error) {
	raw, err := m.rpc.Call(ctx, m.cfg.Token, m.abi.PackBalanceOf(m.key.Address))
	if err != nil {
		return nil, err
	}
	return m.abi.UnpackBalanceOf(raw)
}

// belowFloor reports whether the master is under gas.floor_wei, and what it
// holds. An unset or zero floor means there is nothing to be below, which reads
// as "fine" rather than as "always trade".
func (m *masterCtx) belowFloor(ctx context.Context) (bool, *big.Int, error) {
	native, err := m.rpc.BalanceBNB(ctx, m.key.Address)
	if err != nil {
		return false, nil, err
	}
	return m.wouldTrade(native), native, nil
}

// wouldTrade is the decision on its own, so it can be tested without a chain.
// An unset or zero floor means there is nothing to be below, which reads as
// "fine" rather than as "always trade" — getting that backwards would make a
// cron line trade on every run.
func (m *masterCtx) wouldTrade(native *big.Int) bool {
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

	low := m.cfg.GasFloor != nil && m.cfg.GasFloor.Sign() > 0 && native.Cmp(m.cfg.GasFloor) < 0
	if low {
		short := new(big.Int).Sub(m.cfg.GasFloor, native)
		fmt.Fprintf(w, "\nBELOW THE GAS FLOOR — the service never refills this itself.\n")
		fmt.Fprintf(w, "Run:  bsc swap --buy-bnb %s\n", dec(short, 18))
	}

	// A price in both directions, so the operator sees what a trade would
	// actually fetch rather than a mid-market number that no router offers.
	router, wrapped, err := m.router(ctx)
	if err != nil {
		fmt.Fprintf(w, "\nprice      unavailable: %v\n", err)
		if low {
			*code = 1
		}
		return nil
	}
	fmt.Fprintf(w, "router     %s\n", router.Hex())
	oneToken := pow10(m.decimals)
	if out, err := m.quoteOut(ctx, router, oneToken, []common.Address{m.cfg.Token, wrapped}); err == nil {
		fmt.Fprintf(w, "price      1 token -> %s native\n", dec(out, 18))
	}
	oneNative := pow10(18)
	if out, err := m.quoteOut(ctx, router, oneNative, []common.Address{wrapped, m.cfg.Token}); err == nil {
		fmt.Fprintf(w, "           1 native -> %s tokens\n", dec(out, m.decimals))
	}
	if low {
		*code = 1
	}
	return nil
}

// quoteOut asks what `in` would fetch: the last amount in the router's answer.
func (m *masterCtx) quoteOut(ctx context.Context, router common.Address, in *big.Int, path []common.Address) (*big.Int, error) {
	amounts, err := m.amounts(ctx, router, swap.PackGetAmountsOut(in, path))
	if err != nil {
		return nil, err
	}
	return amounts[len(amounts)-1], nil
}

// quoteIn asks what receiving `out` would cost: the first amount in the answer.
func (m *masterCtx) quoteIn(ctx context.Context, router common.Address, out *big.Int, path []common.Address) (*big.Int, error) {
	amounts, err := m.amounts(ctx, router, swap.PackGetAmountsIn(out, path))
	if err != nil {
		return nil, err
	}
	return amounts[0], nil
}

func (m *masterCtx) amounts(ctx context.Context, router common.Address, data []byte) ([]*big.Int, error) {
	raw, err := m.rpc.Call(ctx, router, data)
	if err != nil {
		return nil, err
	}
	amounts, err := swap.UnpackAmounts(raw)
	if err != nil {
		return nil, err
	}
	if len(amounts) < 2 {
		return nil, fmt.Errorf("router returned %d amounts, want at least 2", len(amounts))
	}
	return amounts, nil
}

func (m *masterCtx) swap(ctx context.Context, cmd *cobra.Command, sellUSDT, buyBNB string, ifBelow, dryRun, yes bool, code *int) error {
	w := cmd.OutOrStdout()

	// The guard that makes this runnable on a timer. It is checked before the
	// router is even dialled, so the common case — the master is fine — costs
	// one balance read and says so (§44).
	if ifBelow {
		below, native, err := m.belowFloor(ctx)
		if err != nil {
			*code = 1
			return err
		}
		if !below {
			fmt.Fprintf(w, "native %s is at or above the floor %s — nothing to do\n",
				dec(native, 18), dec(m.cfg.GasFloor, 18))
			return nil
		}
		fmt.Fprintf(w, "native %s is below the floor %s\n", dec(native, 18), dec(m.cfg.GasFloor, 18))
	}

	router, wrapped, err := m.router(ctx)
	if err != nil {
		*code = 1
		return err
	}
	// One direction only: tokens in, native out. The master spends gas and does
	// not earn tokens on its own any more, so the only trade it ever needs is
	// turning a float the operator put there into the gas it runs on (§38).
	path := []common.Address{m.cfg.Token, wrapped}

	var (
		amountIn  *big.Int // what leaves the master
		amountOut *big.Int // what it receives
		data      []byte
	)
	deadline := big.NewInt(time.Now().Add(m.cfg.SwapDeadline).Unix())

	if sellUSDT != "" {
		if amountIn, err = parseDec(sellUSDT, m.decimals); err != nil {
			return fmt.Errorf("--sell-usdt: %w", err)
		}
		if amountOut, err = m.quoteOut(ctx, router, amountIn, path); err != nil {
			*code = 1
			return err
		}
		minOut, err := swap.MinOut(amountOut, m.cfg.SwapSlippage)
		if err != nil {
			*code = 1
			return err
		}
		fmt.Fprintf(w, "spend    %s tokens (exact)\n", dec(amountIn, m.decimals))
		fmt.Fprintf(w, "receive  %s native\n", dec(amountOut, 18))
		fmt.Fprintf(w, "at least %s native  (%d bps slippage)\n", dec(minOut, 18), m.cfg.SwapSlippage)
		data = swap.PackSwapExactTokensForETH(amountIn, minOut, path, m.key.Address, deadline)
	} else {
		if amountOut, err = parseDec(buyBNB, 18); err != nil {
			return fmt.Errorf("--buy-bnb: %w", err)
		}
		if amountIn, err = m.quoteIn(ctx, router, amountOut, path); err != nil {
			*code = 1
			return err
		}
		maxIn, err := swap.MaxIn(amountIn, m.cfg.SwapSlippage)
		if err != nil {
			*code = 1
			return err
		}
		fmt.Fprintf(w, "receive  %s native (exact)\n", dec(amountOut, 18))
		fmt.Fprintf(w, "spend    %s tokens\n", dec(amountIn, m.decimals))
		fmt.Fprintf(w, "at most  %s tokens  (%d bps slippage)\n", dec(maxIn, m.decimals), m.cfg.SwapSlippage)
		// The allowance and the balance check both have to cover the ceiling,
		// not the quote: the router may pull up to maxIn.
		amountIn = maxIn
		data = swap.PackSwapTokensForExactETH(amountOut, maxIn, path, m.key.Address, deadline)
	}
	held, err := m.tokenBalance(ctx)
	if err != nil {
		*code = 1
		return err
	}
	if held.Cmp(amountIn) < 0 {
		*code = 1
		return fmt.Errorf("the master holds %s tokens, which is less than the %s this trade needs",
			dec(held, m.decimals), dec(amountIn, m.decimals))
	}

	if dryRun {
		return nil
	}
	if !yes && !confirm(cmd, "proceed?") {
		fmt.Fprintln(w, "cancelled")
		return nil
	}
	if err := m.ensureAllowance(ctx, w, router, amountIn); err != nil {
		*code = 1
		return err
	}
	hash, err := m.send(ctx, router, new(big.Int), data)
	if err != nil {
		*code = 1
		return err
	}
	fmt.Fprintf(w, "sent     %s\n", hash.Hex())
	if err := m.await(ctx, hash); err != nil {
		*code = 1
		return err
	}
	fmt.Fprintln(w, "confirmed")
	return nil
}

// ensureAllowance grants the router an unlimited allowance if it does not have
// one, mirroring how managed wallets approve the master: granted once, and
// skipped when already in place.
func (m *masterCtx) ensureAllowance(ctx context.Context, w io.Writer, router common.Address, need *big.Int) error {
	raw, err := m.rpc.Call(ctx, m.cfg.Token, m.abi.PackAllowance(m.key.Address, router))
	if err != nil {
		return err
	}
	allowance, err := m.abi.UnpackAllowance(raw)
	if err != nil {
		return err
	}
	if allowance.Cmp(need) >= 0 {
		return nil
	}
	fmt.Fprintln(w, "approve  granting the router an allowance first")
	hash, err := m.send(ctx, m.cfg.Token, new(big.Int), m.abi.PackApprove(router, maxUint256()))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "         %s\n", hash.Hex())
	// The swap must not be built against an allowance that has not landed.
	return m.await(ctx, hash)
}

func (m *masterCtx) send(ctx context.Context, to common.Address, value *big.Int, data []byte) (common.Hash, error) {
	nonce, err := m.rpc.Nonce(ctx, m.key.Address)
	if err != nil {
		return common.Hash{}, err
	}
	gasPrice, err := m.rpc.GasPrice(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	gasPrice = bumped(gasPrice, m.cfg.GasMultiplier)
	gas, err := m.rpc.EstimateGas(ctx, callMsg(m.key.Address, to, value, data))
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimating gas (the trade would revert): %w", err)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: &to, Value: value,
		Gas: gas * 12 / 10, GasPrice: gasPrice, Data: data,
	})
	signed, err := types.SignTx(tx, types.NewEIP155Signer(new(big.Int).SetUint64(m.cfg.ChainID)), m.key.Priv)
	if err != nil {
		return common.Hash{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return common.Hash{}, err
	}
	return m.rpc.SendRawTx(ctx, raw)
}

// await blocks until a transaction is mined. This command is attended and
// short-lived, so it waits rather than journalling: there is no flow to resume
// and a human is watching the output.
func (m *masterCtx) await(ctx context.Context, hash common.Hash) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for %s", hash.Hex())
		case <-time.After(2 * time.Second):
		}
		rc, err := m.rpc.Receipt(ctx, hash)
		if err != nil || rc == nil {
			continue
		}
		if rc.Status != types.ReceiptStatusSuccessful {
			return fmt.Errorf("transaction %s reverted", hash.Hex())
		}
		return nil
	}
}
