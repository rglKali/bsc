// Package config resolves bsc's configuration from a YAML file and the
// environment.
//
// There is one config for the whole service, replacing v1's three separate
// matrices. Anything that can be derived is derived rather than configured:
// gas amounts come from estimates, the fee collector defaults to the master
// address, and the starting block defaults to the current finalized head.
//
// **The split between the file and the environment is a security boundary, not
// a convenience.** `master_secret` is the one setting that can move every app's
// money, so it belongs in a secrets manager and reaches the process as
// BSC_MASTER_SECRET. Everything else is operational — thresholds, intervals,
// fee policy — and belongs in a file you can review, diff and keep in version
// control. Putting the two in one file is what forces the whole thing to be a
// secret, and with it the answer to "what changed last month" (§30).
//
// Keys are nested, and an environment variable is the key path in upper case
// with BSC_ in front: swap.amount_wei is BSC_SWAP_AMOUNT_WEI. Precedence is
// flag, then environment, then file, then default.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"strings"
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
	DBPath   string // db_path — the only datastore
	HTTPAddr string // http_addr — the only listener

	// UIEnabled mounts the local dashboard at /ui/. Off by default, and that
	// default is the security boundary: the listener has no authentication, so
	// on a host where it is reachable by anyone else this hands them every
	// app's money. It is a sandbox and an operator's window, not a product
	// surface (§29).
	UIEnabled bool // ui_enabled

	MasterSecret string // BSC_MASTER_SECRET (32-byte hex; required; environment only)
	RPCURL       string // chain.rpc_url — the one chain input; everything else follows it
	RPCRateLimit int    // chain.rpc_rate_limit — one budget shared by everything

	// ChainID and Token are *resolved*, not configured: ResolveChain fills them
	// in once the endpoint has said which chain it is. Token may be overridden
	// with chain.token_address, which is the only reason it is read here at all.
	ChainID uint64
	Token   common.Address

	StartBlock    uint64        // chain.start_block — only on a fresh database; 0 = the finalized head
	PollInterval  time.Duration // chain.poll_interval
	BackfillBatch int           // chain.backfill_batch — blocks per write transaction while catching up
	MaxLagBlocks  uint64        // chain.max_lag_blocks — refuse withdrawals past this

	// DrainThreshold is pure gas economics: don't spend a transaction moving
	// less than this. It says nothing about what an app is credited — a deposit
	// worth a whole cent is always recorded, and shows as pending until its
	// drain is worth running (§22).
	DrainThreshold *big.Int // money.drain_threshold_wei

	// HouseSweepMin is how much excess over an app's ledger — fees, sub-cent
	// dust, stray transfers — is worth one transfer to collect. Zero disables
	// sweeping, which is safe: the money is ours either way and simply
	// accumulates in the wallet.
	HouseSweepMin money.Cents // money.house_sweep_min_cents

	DefaultFee   money.Cents // money.default_fee_cents — a new app's withdrawal fee
	FeeCollector string      // money.fee_collector — empty means the master

	// Gas top-up: swapping collected fees back into gas. On by default — a
	// gateway that runs out of gas stops completely — but it is still the only
	// thing the service does on its own initiative, so it is bounded on every
	// side and can be turned off outright.
	SwapEnabled  bool          // swap.enabled
	SwapRouter   string        // swap.router — a Uniswap-V2-style router; defaults per chain
	SwapNative   string        // swap.wrapped_native — optional override; the router is asked otherwise
	SwapAmount   *big.Int      // swap.amount_wei — tokens traded per top-up
	GasFloor     *big.Int      // swap.gas_floor_wei — native balance below which a top-up is due
	SwapSlippage uint32        // swap.slippage_bps
	SwapCooldown time.Duration // swap.cooldown — minimum gap between attempts

	FundingMultiplier float64       // gas.funding_multiplier — the only gas knobs
	GasMultiplier     float64       // gas.price_multiplier
	RebroadcastAfter  time.Duration // gas.rebroadcast_after
	MasterPoll        time.Duration // gas.master_poll — how often to refresh the BNB gauge

	SnapshotDir      string        // snapshot.dir — empty disables self-backup
	SnapshotInterval time.Duration // snapshot.interval
	SnapshotKeep     int           // snapshot.keep
}

// oneUSDT is 10^18 wei: USDT on BSC has 18 decimals.
var oneUSDT, _ = new(big.Int).SetString("1000000000000000000", 10)

// DefaultPath is where a deployment normally keeps its configuration. The
// systemd unit passes it explicitly; nothing reads it implicitly.
const DefaultPath = "/etc/bsc/config.yaml"

// EnvPrefix is prepended to every key path to form an environment variable:
// swap.amount_wei is BSC_SWAP_AMOUNT_WEI.
const EnvPrefix = "BSC"

// EnvVar renders the environment variable that sets a key, which is what error
// messages should name: an operator who set the wrong thing needs to be told
// the spelling they used, and both spellings address the same setting.
func EnvVar(key string) string {
	return EnvPrefix + "_" + strings.ToUpper(strings.NewReplacer(".", "_").Replace(key))
}

// New returns a viper pre-loaded with bsc's defaults and reading the
// environment. A CLI binds its flags to this before calling Parse, so a flag
// beats an environment variable which beats the file which beats the default.
func New() *viper.Viper {
	v := viper.New()
	v.SetDefault("db_path", "bsc.db")
	v.SetDefault("http_addr", "127.0.0.1:8800")
	v.SetDefault("ui_enabled", false) // see Config.UIEnabled
	v.SetDefault("chain.rpc_url", "") // resolved from the chain the endpoint reports
	// At ~0.45s blocks the watcher alone needs ~2.2 req/s sustained, and it has
	// to outrun the chain to ever catch up after an outage — see docs/REWRITE.md §10.
	v.SetDefault("chain.rpc_rate_limit", 20)
	v.SetDefault("chain.token_address", "") // resolved from the chain the endpoint reports
	v.SetDefault("chain.start_block", 0)
	v.SetDefault("chain.poll_interval", 500*time.Millisecond)
	v.SetDefault("chain.backfill_batch", 100)
	v.SetDefault("chain.max_lag_blocks", 200)
	v.SetDefault("money.drain_threshold_wei", oneUSDT.String())
	v.SetDefault("money.default_fee_cents", 100)     // $1.00
	v.SetDefault("money.house_sweep_min_cents", 100) // $1.00 of excess is worth a transfer
	v.SetDefault("money.fee_collector", "")
	v.SetDefault("gas.funding_multiplier", 1.25)
	v.SetDefault("gas.price_multiplier", 1.10)
	v.SetDefault("gas.rebroadcast_after", 2*time.Minute)
	v.SetDefault("gas.master_poll", 30*time.Second)
	v.SetDefault("swap.enabled", true)
	v.SetDefault("swap.router", "") // resolved from the chain the endpoint reports
	v.SetDefault("swap.wrapped_native", "")
	v.SetDefault("swap.amount_wei", new(big.Int).Mul(oneUSDT, big.NewInt(10)).String())
	// A transfer costs well under a thousandth of a BNB, so this floor is a few
	// hundred transactions of headroom — enough that a top-up has time to land
	// before anything actually runs dry.
	v.SetDefault("swap.gas_floor_wei", "50000000000000000") // 0.05
	v.SetDefault("swap.slippage_bps", 100)                  // 1%
	v.SetDefault("swap.cooldown", time.Hour)
	v.SetDefault("snapshot.dir", "")
	v.SetDefault("snapshot.interval", time.Hour)
	v.SetDefault("snapshot.keep", 24)
	v.SetDefault("log.level", "info")

	// Nested keys become underscored environment variables, so every setting is
	// addressable both ways and neither spelling is second class.
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// AutomaticEnv only consults the environment for keys viper already knows,
	// and master_secret has no default precisely because it must never have one.
	// Binding it explicitly is what makes BSC_MASTER_SECRET reachable.
	_ = v.BindEnv("master_secret")
	return v
}

// ReadFile overlays a YAML configuration onto v. A missing file at the default
// path is not an error — every setting has a default and the environment can
// carry the rest — but a file the operator named explicitly and that cannot be
// read is, because silently ignoring it would run the service on settings
// nobody chose.
func ReadFile(v *viper.Viper, path string, explicit bool) error {
	if path == "" {
		return nil
	}
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}
	return nil
}

// Load reads the configuration from the environment alone.
func Load() (Config, error) { return Parse(New()) }

// Parse resolves and validates a configuration from an already-populated viper.
func Parse(v *viper.Viper) (Config, error) {
	cfg := Config{
		DBPath:            v.GetString("db_path"),
		HTTPAddr:          v.GetString("http_addr"),
		UIEnabled:         v.GetBool("ui_enabled"),
		MasterSecret:      v.GetString("master_secret"),
		RPCURL:            v.GetString("chain.rpc_url"),
		RPCRateLimit:      v.GetInt("chain.rpc_rate_limit"),
		StartBlock:        v.GetUint64("chain.start_block"),
		PollInterval:      v.GetDuration("chain.poll_interval"),
		BackfillBatch:     v.GetInt("chain.backfill_batch"),
		MaxLagBlocks:      v.GetUint64("chain.max_lag_blocks"),
		FeeCollector:      v.GetString("money.fee_collector"),
		FundingMultiplier: v.GetFloat64("gas.funding_multiplier"),
		GasMultiplier:     v.GetFloat64("gas.price_multiplier"),
		RebroadcastAfter:  v.GetDuration("gas.rebroadcast_after"),
		MasterPoll:        v.GetDuration("gas.master_poll"),
		SwapEnabled:       v.GetBool("swap.enabled"),
		SwapRouter:        v.GetString("swap.router"),
		SwapNative:        v.GetString("swap.wrapped_native"),
		SwapSlippage:      uint32(v.GetUint64("swap.slippage_bps")),
		SwapCooldown:      v.GetDuration("swap.cooldown"),
		SnapshotDir:       v.GetString("snapshot.dir"),
		SnapshotInterval:  v.GetDuration("snapshot.interval"),
		SnapshotKeep:      v.GetInt("snapshot.keep"),
	}

	if cfg.MasterSecret == "" {
		return Config{}, fmt.Errorf("master_secret is required: set %s in the environment (never in the config file)", EnvVar("master_secret"))
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
	if token := v.GetString("chain.token_address"); token != "" {
		if !common.IsHexAddress(token) {
			return Config{}, fmt.Errorf("chain.token_address (%s) %q is not a hex address", EnvVar("chain.token_address"), token)
		}
		cfg.Token = common.HexToAddress(token)
	}

	if cfg.FeeCollector != "" && !common.IsHexAddress(cfg.FeeCollector) {
		return Config{}, fmt.Errorf("money.fee_collector (%s) %q is not a hex address", EnvVar("money.fee_collector"), cfg.FeeCollector)
	}

	var err error
	if cfg.DrainThreshold, err = wei(v, "money.drain_threshold_wei"); err != nil {
		return Config{}, err
	}
	if cfg.SwapAmount, err = wei(v, "swap.amount_wei"); err != nil {
		return Config{}, err
	}
	if cfg.GasFloor, err = wei(v, "swap.gas_floor_wei"); err != nil {
		return Config{}, err
	}
	if cfg.SwapEnabled {
		// A router the operator did not name is resolved from the chain in
		// ResolveChain; only an explicit one can be checked this early.
		if cfg.SwapRouter != "" && !common.IsHexAddress(cfg.SwapRouter) {
			return Config{}, fmt.Errorf("swap.router (%s) %q is not a hex address", EnvVar("swap.router"), cfg.SwapRouter)
		}
		// Normally unset: the router reports its own wrapped token, which cannot
		// then disagree with it. Only validated when overridden.
		if cfg.SwapNative != "" && !common.IsHexAddress(cfg.SwapNative) {
			return Config{}, fmt.Errorf("swap.wrapped_native (%s) %q is not a hex address", EnvVar("swap.wrapped_native"), cfg.SwapNative)
		}
		if cfg.SwapAmount.Sign() <= 0 || cfg.GasFloor.Sign() <= 0 {
			return Config{}, errors.New("swap.amount_wei and swap.gas_floor_wei must be positive when swapping is enabled")
		}
		if cfg.SwapSlippage >= 10_000 {
			return Config{}, fmt.Errorf("swap.slippage_bps %d would accept any price at all", cfg.SwapSlippage)
		}
		if cfg.SwapCooldown <= 0 {
			return Config{}, errors.New("swap.cooldown must be positive: it is what bounds a losing swap loop")
		}
	}
	cfg.DefaultFee = money.Cents(v.GetInt64("money.default_fee_cents"))
	cfg.HouseSweepMin = money.Cents(v.GetInt64("money.house_sweep_min_cents"))
	if cfg.DefaultFee < 0 {
		return Config{}, errors.New("money.default_fee_cents must not be negative")
	}
	if cfg.HouseSweepMin < 0 {
		return Config{}, errors.New("money.house_sweep_min_cents must not be negative")
	}

	// The rate limit has to clear the block rate by a wide margin or the
	// watcher can never catch up after an outage. Refusing a value that cannot
	// keep up beats discovering it during an incident.
	if cfg.RPCRateLimit < 5 {
		return Config{}, fmt.Errorf(
			"chain.rpc_rate_limit %d is below 5/s; at ~2.2 blocks/s the watcher could not outrun the chain",
			cfg.RPCRateLimit)
	}
	if cfg.BackfillBatch <= 0 {
		return Config{}, fmt.Errorf("chain.backfill_batch must be positive (got %d)", cfg.BackfillBatch)
	}
	if cfg.FundingMultiplier < 1 || cfg.GasMultiplier < 1 {
		return Config{}, errors.New("gas.funding_multiplier and gas.price_multiplier must be at least 1.0")
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
// This is a separate step from Parse because Parse does no I/O — it must be
// testable and it must fail on a bad value before anything dials — while the
// chain id is, by design, something only the chain can tell us. The alternative
// was a CHAIN_ID setting, and a setting that can disagree with the endpoint is
// one that eventually will: the signer binds every transaction to it, so a
// mismatch leaves a service that reads blocks perfectly and cannot send
// anything, with nothing in the logs to explain it.
//
// An explicit chain.token_address or swap.router still wins; this only supplies what
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
				"chain.token_address (%s) must be set: %s is chain %d, which has no default token",
				EnvVar("chain.token_address"), c.RPCURL, chainID)
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
			"no default swap router for chain %d: set swap.router (%s), or swap.enabled=false (%s=false) to run without gas top-ups",
			chainID, EnvVar("swap.router"), EnvVar("swap.enabled"))
	}
	return nil
}
