// Package config resolves bsc's configuration from the environment.
//
// There is one config for the whole service, replacing v1's three separate
// matrices. Anything that can be derived is derived rather than configured:
// gas amounts come from estimates, the fee collector defaults to the master
// address, and the starting block defaults to the current finalized head.
package config

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"bsc/chain"
	"bsc/money"
	"bsc/swap"
	"bsc/usdt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/spf13/viper"
)

// Config is the whole service's runtime configuration.
type Config struct {
	DBPath   string // DB_PATH — the only datastore
	HTTPAddr string // HTTP_ADDR — the only listener

	MasterSecret string // MASTER_SECRET (32-byte hex; required)
	RPCURL       string // RPC_URL — the one chain input; everything else follows it
	RPCRateLimit int    // RPC_RATE_LIMIT — one budget shared by everything

	// ChainID and Token are *resolved*, not configured: ResolveChain fills them
	// in once the endpoint has said which chain it is. Token may be overridden
	// with TOKEN_ADDRESS, which is the only reason it is read here at all.
	ChainID uint64
	Token   common.Address

	StartBlock    uint64        // START_BLOCK — only on a fresh database; 0 = the finalized head
	PollInterval  time.Duration // POLL_INTERVAL
	BackfillBatch int           // BACKFILL_BATCH — blocks per write transaction while catching up
	MaxLagBlocks  uint64        // MAX_LAG_BLOCKS — refuse withdrawals past this

	// DrainThreshold is pure gas economics: don't spend a transaction moving
	// less than this. It says nothing about what an app is credited — a deposit
	// worth a whole cent is always recorded, and shows as pending until its
	// drain is worth running (§22).
	DrainThreshold *big.Int // DRAIN_THRESHOLD_WEI

	// HouseSweepMin is how much excess over an app's ledger — fees, sub-cent
	// dust, stray transfers — is worth one transfer to collect. Zero disables
	// sweeping, which is safe: the money is ours either way and simply
	// accumulates in the wallet.
	HouseSweepMin money.Cents // HOUSE_SWEEP_MIN_CENTS

	DefaultFee   money.Cents // DEFAULT_FEE_CENTS — a new app's withdrawal fee
	FeeCollector string      // FEE_COLLECTOR — empty means the master

	// Gas top-up: swapping collected fees back into gas. On by default — a
	// gateway that runs out of gas stops completely — but it is still the only
	// thing the service does on its own initiative, so it is bounded on every
	// side and can be turned off outright.
	SwapEnabled  bool          // SWAP_ENABLED
	SwapRouter   string        // SWAP_ROUTER — a Uniswap-V2-style router; defaults per chain
	SwapNative   string        // SWAP_WRAPPED_NATIVE — optional override; the router is asked otherwise
	SwapAmount   *big.Int      // SWAP_AMOUNT_WEI — tokens traded per top-up
	GasFloor     *big.Int      // GAS_FLOOR_WEI — native balance below which a top-up is due
	SwapSlippage uint32        // SWAP_SLIPPAGE_BPS
	SwapCooldown time.Duration // SWAP_COOLDOWN — minimum gap between attempts

	FundingMultiplier float64       // FUNDING_MULTIPLIER — the only gas knobs
	GasMultiplier     float64       // GAS_PRICE_MULTIPLIER
	RebroadcastAfter  time.Duration // REBROADCAST_AFTER
	MasterPoll        time.Duration // MASTER_POLL — how often to refresh the BNB gauge

	SnapshotDir      string        // SNAPSHOT_DIR — empty disables self-backup
	SnapshotInterval time.Duration // SNAPSHOT_INTERVAL
	SnapshotKeep     int           // SNAPSHOT_KEEP
}

// oneUSDT is 10^18 wei: USDT on BSC has 18 decimals.
var oneUSDT, _ = new(big.Int).SetString("1000000000000000000", 10)

// New returns a viper pre-loaded with bsc's defaults and reading the
// environment. A CLI binds its flags to this before calling Parse, so a flag
// beats an environment variable which beats the default.
func New() *viper.Viper {
	v := viper.New()
	v.SetDefault("db_path", "bsc.db")
	v.SetDefault("http_addr", "127.0.0.1:8800")
	v.SetDefault("rpc_url", "") // resolved from the chain the endpoint reports
	// At ~0.45s blocks the watcher alone needs ~2.2 req/s sustained, and it has
	// to outrun the chain to ever catch up after an outage — see docs/REWRITE.md §10.
	v.SetDefault("rpc_rate_limit", 20)
	v.SetDefault("token_address", "") // resolved from the chain the endpoint reports
	v.SetDefault("start_block", 0)
	v.SetDefault("poll_interval", 500*time.Millisecond)
	v.SetDefault("backfill_batch", 100)
	v.SetDefault("max_lag_blocks", 200)
	v.SetDefault("drain_threshold_wei", oneUSDT.String())
	v.SetDefault("default_fee_cents", 100)     // $1.00
	v.SetDefault("house_sweep_min_cents", 100) // $1.00 of excess is worth a transfer
	v.SetDefault("fee_collector", "")
	v.SetDefault("funding_multiplier", 1.25)
	v.SetDefault("gas_price_multiplier", 1.10)
	v.SetDefault("rebroadcast_after", 2*time.Minute)
	v.SetDefault("master_poll", 30*time.Second)
	v.SetDefault("swap_enabled", true)
	v.SetDefault("swap_router", "") // resolved from the chain the endpoint reports
	v.SetDefault("swap_wrapped_native", "")
	v.SetDefault("swap_amount_wei", new(big.Int).Mul(oneUSDT, big.NewInt(10)).String())
	// A transfer costs well under a thousandth of a BNB, so this floor is a few
	// hundred transactions of headroom — enough that a top-up has time to land
	// before anything actually runs dry.
	v.SetDefault("gas_floor_wei", "50000000000000000") // 0.05
	v.SetDefault("swap_slippage_bps", 100)             // 1%
	v.SetDefault("swap_cooldown", time.Hour)
	v.SetDefault("snapshot_dir", "")
	v.SetDefault("snapshot_interval", time.Hour)
	v.SetDefault("snapshot_keep", 24)
	v.AutomaticEnv()
	return v
}

// Load reads the configuration from the environment alone.
func Load() (Config, error) { return Parse(New()) }

// Parse resolves and validates a configuration from an already-populated viper.
func Parse(v *viper.Viper) (Config, error) {
	cfg := Config{
		DBPath:            v.GetString("db_path"),
		HTTPAddr:          v.GetString("http_addr"),
		MasterSecret:      v.GetString("master_secret"),
		RPCURL:            v.GetString("rpc_url"),
		RPCRateLimit:      v.GetInt("rpc_rate_limit"),
		StartBlock:        v.GetUint64("start_block"),
		PollInterval:      v.GetDuration("poll_interval"),
		BackfillBatch:     v.GetInt("backfill_batch"),
		MaxLagBlocks:      v.GetUint64("max_lag_blocks"),
		FeeCollector:      v.GetString("fee_collector"),
		FundingMultiplier: v.GetFloat64("funding_multiplier"),
		GasMultiplier:     v.GetFloat64("gas_price_multiplier"),
		RebroadcastAfter:  v.GetDuration("rebroadcast_after"),
		MasterPoll:        v.GetDuration("master_poll"),
		SwapEnabled:       v.GetBool("swap_enabled"),
		SwapRouter:        v.GetString("swap_router"),
		SwapNative:        v.GetString("swap_wrapped_native"),
		SwapSlippage:      uint32(v.GetUint64("swap_slippage_bps")),
		SwapCooldown:      v.GetDuration("swap_cooldown"),
		SnapshotDir:       v.GetString("snapshot_dir"),
		SnapshotInterval:  v.GetDuration("snapshot_interval"),
		SnapshotKeep:      v.GetInt("snapshot_keep"),
	}

	if cfg.MasterSecret == "" {
		return Config{}, errors.New("MASTER_SECRET is required")
	}

	// The endpoint is the one chain input, so it is the one with a literal
	// default. Everything that used to follow a configured CHAIN_ID now follows
	// the chain the endpoint actually reports — see ResolveChain.
	if cfg.RPCURL == "" {
		cfg.RPCURL = chain.MainnetDefaultRPC
	}

	// Validated here, resolved in ResolveChain: a bad address should be refused
	// before anything dials, but a *missing* one cannot be filled in until the
	// chain has identified itself.
	if token := v.GetString("token_address"); token != "" {
		if !common.IsHexAddress(token) {
			return Config{}, fmt.Errorf("TOKEN_ADDRESS %q is not a hex address", token)
		}
		cfg.Token = common.HexToAddress(token)
	}

	if cfg.FeeCollector != "" && !common.IsHexAddress(cfg.FeeCollector) {
		return Config{}, fmt.Errorf("FEE_COLLECTOR %q is not a hex address", cfg.FeeCollector)
	}

	var err error
	if cfg.DrainThreshold, err = wei(v, "drain_threshold_wei"); err != nil {
		return Config{}, err
	}
	if cfg.SwapAmount, err = wei(v, "swap_amount_wei"); err != nil {
		return Config{}, err
	}
	if cfg.GasFloor, err = wei(v, "gas_floor_wei"); err != nil {
		return Config{}, err
	}
	if cfg.SwapEnabled {
		// A router the operator did not name is resolved from the chain in
		// ResolveChain; only an explicit one can be checked this early.
		if cfg.SwapRouter != "" && !common.IsHexAddress(cfg.SwapRouter) {
			return Config{}, fmt.Errorf("SWAP_ROUTER %q is not a hex address", cfg.SwapRouter)
		}
		// Normally unset: the router reports its own wrapped token, which cannot
		// then disagree with it. Only validated when overridden.
		if cfg.SwapNative != "" && !common.IsHexAddress(cfg.SwapNative) {
			return Config{}, fmt.Errorf("SWAP_WRAPPED_NATIVE %q is not a hex address", cfg.SwapNative)
		}
		if cfg.SwapAmount.Sign() <= 0 || cfg.GasFloor.Sign() <= 0 {
			return Config{}, errors.New("SWAP_AMOUNT_WEI and GAS_FLOOR_WEI must be positive when swapping is enabled")
		}
		if cfg.SwapSlippage >= 10_000 {
			return Config{}, fmt.Errorf("SWAP_SLIPPAGE_BPS %d would accept any price at all", cfg.SwapSlippage)
		}
		if cfg.SwapCooldown <= 0 {
			return Config{}, errors.New("SWAP_COOLDOWN must be positive: it is what bounds a losing swap loop")
		}
	}
	cfg.DefaultFee = money.Cents(v.GetInt64("default_fee_cents"))
	cfg.HouseSweepMin = money.Cents(v.GetInt64("house_sweep_min_cents"))
	if cfg.DefaultFee < 0 {
		return Config{}, errors.New("DEFAULT_FEE_CENTS must not be negative")
	}
	if cfg.HouseSweepMin < 0 {
		return Config{}, errors.New("HOUSE_SWEEP_MIN_CENTS must not be negative")
	}

	// The rate limit has to clear the block rate by a wide margin or the
	// watcher can never catch up after an outage. Refusing a value that cannot
	// keep up beats discovering it during an incident.
	if cfg.RPCRateLimit < 5 {
		return Config{}, fmt.Errorf(
			"RPC_RATE_LIMIT %d is below 5/s; at ~2.2 blocks/s the watcher could not outrun the chain",
			cfg.RPCRateLimit)
	}
	if cfg.BackfillBatch <= 0 {
		return Config{}, fmt.Errorf("BACKFILL_BATCH must be positive (got %d)", cfg.BackfillBatch)
	}
	if cfg.FundingMultiplier < 1 || cfg.GasMultiplier < 1 {
		return Config{}, errors.New("FUNDING_MULTIPLIER and GAS_PRICE_MULTIPLIER must be at least 1.0")
	}
	return cfg, nil
}

// wei parses an amount that must be a non-negative integer in wei.
func wei(v *viper.Viper, key string) (*big.Int, error) {
	raw := v.GetString(key)
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok || n.Sign() < 0 {
		return nil, fmt.Errorf("%s must be a non-negative integer in wei (got %q)", key, raw)
	}
	return n, nil
}

// ResolveChain fills in everything that depends on which chain we are actually
// talking to, using the id the endpoint reported after connecting.
//
// This is a separate step from Load because Load does no I/O — it must be
// testable and it must fail on a bad value before anything dials — while the
// chain id is, by design, something only the chain can tell us. The alternative
// was a CHAIN_ID setting, and a setting that can disagree with the endpoint is
// one that eventually will: the signer binds every transaction to it, so a
// mismatch leaves a service that reads blocks perfectly and cannot send
// anything, with nothing in the logs to explain it.
//
// An explicit TOKEN_ADDRESS or SWAP_ROUTER still wins; this only supplies what
// the operator left out.
func (c *Config) ResolveChain(chainID uint64) error {
	if chainID == 0 {
		return errors.New("config: cannot resolve settings without a chain id")
	}
	c.ChainID = chainID

	// Defaulting the token to the mainnet address everywhere would be quietly
	// wrong on any other chain: the contract would not exist there, and the
	// watcher would report no transfers rather than complaining.
	if c.Token == (common.Address{}) {
		switch chainID {
		case chain.MainnetChainID:
			c.Token = usdt.MainnetAddress
		case chain.TestnetChainID:
			c.Token = usdt.TestnetAddress
		default:
			return fmt.Errorf(
				"TOKEN_ADDRESS must be set: %s is chain %d, which has no default token",
				c.RPCURL, chainID)
		}
	}

	if !c.SwapEnabled || c.SwapRouter != "" {
		return nil
	}
	// Addresses verified against the explorer. An unknown chain is a hard error
	// rather than a silent no-op: quietly not swapping would only be discovered
	// when the master ran dry.
	switch chainID {
	case chain.MainnetChainID:
		c.SwapRouter = swap.PancakeV2Mainnet.Hex()
	case chain.TestnetChainID:
		c.SwapRouter = swap.PancakeV2Testnet.Hex()
	default:
		return fmt.Errorf(
			"no default swap router for chain %d: set SWAP_ROUTER, or SWAP_ENABLED=false to run without gas top-ups",
			chainID)
	}
	return nil
}
