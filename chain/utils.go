package chain

// Chain ids and their public RPC endpoints.
//
// These are defaults, not policy: anything set in the environment wins. They
// exist so the common cases — mainnet and Chapel — need no configuration at
// all, while an unusual deployment stays free to point anywhere.
const (
	MainnetChainID = 56 // BNB Smart Chain
	TestnetChainID = 97 // Chapel
)

var (
	MainnetDefaultRPC = "wss://bsc-rpc.publicnode.com"
	TestnetDefaultRPC = "wss://bsc-testnet-rpc.publicnode.com"
)
