package config

import (
	"bsc/chain"
	"bsc/swap"
	"bsc/usdt"

	"math/big"
	"strings"
	"testing"
	"time"
)

const secret = "1111111111111111111111111111111111111111111111111111111111111111"

// withEnv sets MASTER_SECRET plus any extras, cleaned up by t.Setenv.
func withEnv(t *testing.T, kv ...string) {
	t.Helper()
	t.Setenv("MASTER_SECRET", secret)
	for i := 0; i < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func TestDefaults(t *testing.T) {
	withEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "bsc.db" || cfg.HTTPAddr != "127.0.0.1:8800" {
		t.Fatalf("cfg = %+v", cfg)
	}
	// The listener defaults to loopback: this service is never exposed, and a
	// default of :8800 would put an unauthenticated money API on every interface.
	if !strings.HasPrefix(cfg.HTTPAddr, "127.0.0.1") {
		t.Fatalf("default listener %q is not loopback", cfg.HTTPAddr)
	}
	if cfg.PollInterval != 500*time.Millisecond {
		t.Fatalf("poll = %v; at ~0.45s blocks a slower default would add latency to everything", cfg.PollInterval)
	}
	// Zero means "start at the finalized head"; block 0 would scan from genesis.
	if cfg.StartBlock != 0 {
		t.Fatalf("start block = %d", cfg.StartBlock)
	}
	if cfg.DrainThreshold.Cmp(oneUSDT) != 0 {
		t.Fatalf("drain threshold = %s", cfg.DrainThreshold)
	}
	if cfg.DefaultFee != 100 || cfg.HouseSweepMin != 100 {
		t.Fatalf("default fee %d / house sweep min %d", cfg.DefaultFee, cfg.HouseSweepMin)
	}
	if cfg.Token != usdt.MainnetAddress {
		t.Fatalf("token = %s, want mainnet USDT", cfg.Token.Hex())
	}
	if cfg.RPCURL != chain.MainnetDefaultRPC {
		t.Fatalf("rpc = %q, want the mainnet default", cfg.RPCURL)
	}
	if cfg.FeeCollector != "" {
		t.Fatalf("collector = %q, want empty so it resolves to the master", cfg.FeeCollector)
	}
}

func TestMasterSecretIsRequired(t *testing.T) {
	t.Setenv("MASTER_SECRET", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a missing master secret")
	}
}

func TestRateLimitMustOutrunTheChain(t *testing.T) {
	// At ~2.2 blocks/s a limit that cannot clear the block rate leaves the
	// watcher permanently unable to catch up. Refusing at boot beats finding
	// out during an incident.
	withEnv(t, "RPC_RATE_LIMIT", "3")
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted a rate limit below the block rate")
	}
	if !strings.Contains(err.Error(), "outrun") {
		t.Fatalf("error = %v; it should explain why", err)
	}

	withEnv(t, "RPC_RATE_LIMIT", "20")
	if _, err := Load(); err != nil {
		t.Fatalf("a workable limit was rejected: %v", err)
	}
}

func TestValidation(t *testing.T) {
	tests := map[string][2]string{
		"bad token":       {"TOKEN_ADDRESS", "not-an-address"},
		"bad collector":   {"FEE_COLLECTOR", "nope"},
		"bad min deposit": {"DRAIN_THRESHOLD_WEI", "abc"},
		"negative fee":    {"DEFAULT_FEE_CENTS", "-1"},
		"negative sweep":  {"HOUSE_SWEEP_MIN_CENTS", "-1"},
		"zero batch":      {"BACKFILL_BATCH", "0"},
		"low funding mul": {"FUNDING_MULTIPLIER", "0.9"},
		"low gas mul":     {"GAS_PRICE_MULTIPLIER", "0.5"},
	}
	for name, kv := range tests {
		withEnv(t, kv[0], kv[1])
		if _, err := Load(); err == nil {
			t.Fatalf("%s: Load accepted %s=%q", name, kv[0], kv[1])
		}
	}
}

func TestOverrides(t *testing.T) {
	withEnv(t,
		"DB_PATH", "/var/lib/bsc/bsc.db",
		"HTTP_ADDR", "127.0.0.1:9000",
		"START_BLOCK", "48210577",
		"DRAIN_THRESHOLD_WEI", "500",
		"MAX_LAG_BLOCKS", "50",
		"SNAPSHOT_DIR", "/var/backups/bsc",
	)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != "/var/lib/bsc/bsc.db" || cfg.HTTPAddr != "127.0.0.1:9000" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.StartBlock != 48210577 || cfg.MaxLagBlocks != 50 {
		t.Fatalf("start=%d lag=%d", cfg.StartBlock, cfg.MaxLagBlocks)
	}
	if cfg.DrainThreshold.Cmp(big.NewInt(500)) != 0 {
		t.Fatalf("drain threshold = %s", cfg.DrainThreshold)
	}
	if cfg.SnapshotDir != "/var/backups/bsc" {
		t.Fatalf("snapshot dir = %q", cfg.SnapshotDir)
	}
}

func TestMasterSecretAcceptsAPrefixedForm(t *testing.T) {
	// The secret is only parsed downstream, but it must survive config intact.
	withEnv(t, "MASTER_SECRET", "0x"+secret)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MasterSecret != "0x"+secret {
		t.Fatalf("secret was altered")
	}
}

func TestSwappingDefaultsToTheChainsRouter(t *testing.T) {
	// On by default: a gateway that runs out of gas stops completely, so the
	// safe default is the one that keeps it running.
	withEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SwapEnabled {
		t.Fatal("swapping is off by default")
	}
	if cfg.SwapRouter != swap.PancakeV2Mainnet.Hex() {
		t.Fatalf("router = %q, want the mainnet default", cfg.SwapRouter)
	}

	withEnv(t, "CHAIN_ID", "97")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SwapRouter != swap.PancakeV2Testnet.Hex() {
		t.Fatalf("testnet router = %q", cfg.SwapRouter)
	}
}

func TestChainSelectsItsOwnEndpointAndToken(t *testing.T) {
	// Chain id, endpoint and token have to agree. Defaulting the token to
	// mainnet on every chain would be quietly wrong: the contract would not
	// exist, and the watcher would report no transfers rather than an error.
	withEnv(t, "CHAIN_ID", "97")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RPCURL != chain.TestnetDefaultRPC {
		t.Fatalf("rpc = %q, want the testnet default", cfg.RPCURL)
	}
	if cfg.Token != usdt.TestnetAddress {
		t.Fatalf("token = %s, want testnet USDT", cfg.Token.Hex())
	}
}

func TestEnvironmentAlwaysBeatsTheChainDefaults(t *testing.T) {
	// These are defaults, not policy.
	withEnv(t,
		"CHAIN_ID", "97",
		"RPC_URL", "wss://my-own-node.example",
		"TOKEN_ADDRESS", "0x55d398326f99059fF775485246999027B3197955",
	)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RPCURL != "wss://my-own-node.example" {
		t.Fatalf("rpc = %q, want the environment's", cfg.RPCURL)
	}
	if cfg.Token != usdt.MainnetAddress {
		t.Fatalf("token = %s, want the environment's", cfg.Token.Hex())
	}
}

func TestUnknownChainNeedsItsEndpointAndTokenNamed(t *testing.T) {
	// Not loud for its own sake: there is genuinely nothing to dial, and
	// guessing would point a signer at one chain while it signs for another.
	withEnv(t, "CHAIN_ID", "1337", "SWAP_ENABLED", "false")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "RPC_URL") {
		t.Fatalf("err = %v, want it to name RPC_URL", err)
	}

	withEnv(t, "CHAIN_ID", "1337", "SWAP_ENABLED", "false", "RPC_URL", "wss://somewhere.example")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "TOKEN_ADDRESS") {
		t.Fatalf("err = %v, want it to name TOKEN_ADDRESS", err)
	}

	withEnv(t, "CHAIN_ID", "1337", "SWAP_ENABLED", "false",
		"RPC_URL", "wss://somewhere.example",
		"TOKEN_ADDRESS", "0x55d398326f99059fF775485246999027B3197955")
	if _, err := Load(); err != nil {
		t.Fatalf("a fully named unknown chain was rejected: %v", err)
	}
}

func TestUnknownChainMustNameItsRouter(t *testing.T) {
	// Silently not swapping would only be discovered when the master ran dry,
	// so an unresolvable router is a startup failure.
	named := []string{
		"CHAIN_ID", "1337",
		"RPC_URL", "wss://somewhere.example",
		"TOKEN_ADDRESS", "0x55d398326f99059fF775485246999027B3197955",
	}
	withEnv(t, named...)
	_, err := Load()
	if err == nil {
		t.Fatal("accepted an unknown chain with swapping on")
	}
	if !strings.Contains(err.Error(), "SWAP_ENABLED=false") {
		t.Fatalf("error = %v; it should say how to proceed", err)
	}

	// Either naming a router or turning it off is a way forward.
	withEnv(t, append(named, "SWAP_ROUTER", "0x10ED43C718714eb63d5aA57B78B54704E256024E")...)
	if _, err := Load(); err != nil {
		t.Fatalf("explicit router rejected: %v", err)
	}
	withEnv(t, append(named, "SWAP_ENABLED", "false")...)
	if _, err := Load(); err != nil {
		t.Fatalf("disabling rejected: %v", err)
	}
}

func TestSwappingCanBeTurnedOff(t *testing.T) {
	withEnv(t, "SWAP_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SwapEnabled {
		t.Fatal("SWAP_ENABLED=false did not take")
	}
}

func TestEnablingSwapsDemandsCompleteConfiguration(t *testing.T) {
	// Half-specified swapping is worse than none: it would either fail at the
	// worst moment or trade on terms nobody chose.
	router := "0x10ED43C718714eb63d5aA57B78B54704E256024E"

	tests := map[string][]string{
		"bad wrapped native": {"SWAP_ROUTER", router, "SWAP_WRAPPED_NATIVE", "nonsense"},
		"bad router":         {"SWAP_ROUTER", "nonsense"},
		"zero swap amount":   {"SWAP_ROUTER", router, "SWAP_AMOUNT_WEI", "0"},
		"zero floor":         {"SWAP_ROUTER", router, "GAS_FLOOR_WEI", "0"},
		"absurd slippage":    {"SWAP_ROUTER", router, "SWAP_SLIPPAGE_BPS", "10000"},
		"no cooldown":        {"SWAP_ROUTER", router, "SWAP_COOLDOWN", "0s"},
	}
	for name, env := range tests {
		// A subtest per case: t.Setenv restores at the end of the *test*, so a
		// shared loop would leak one case's values into the next.
		t.Run(name, func(t *testing.T) {
			withEnv(t, env...)
			if _, err := Load(); err == nil {
				t.Fatalf("%s: accepted", name)
			}
		})
	}

	var cfg Config
	t.Run("complete", func(t *testing.T) {
		// The wrapped token is deliberately absent: the router reports its own.
		withEnv(t, "SWAP_ROUTER", router)
		var err error
		cfg, err = Load()
		if err != nil {
			t.Fatalf("complete configuration rejected: %v", err)
		}
		if cfg.SwapAmount.Cmp(new(big.Int).Mul(oneUSDT, big.NewInt(10))) != 0 {
			t.Fatalf("swap amount = %s, want 10 tokens", cfg.SwapAmount)
		}
	})
	if cfg.SwapSlippage != 100 {
		t.Fatalf("slippage = %d bps, want a 1%% default", cfg.SwapSlippage)
	}
}
