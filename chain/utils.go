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

// DefaultRPC returns the public endpoint for a known chain.
//
// ok is false for anything else, because guessing would be worse than asking:
// pointing a signer at the wrong chain's node means reading one chain's blocks
// while signing for another, and every transaction would be rejected — after
// the service had already decided what to send.
func DefaultRPC(chainID uint64) (url string, ok bool) {
	switch chainID {
	case MainnetChainID:
		return MainnetDefaultRPC, true
	case TestnetChainID:
		return TestnetDefaultRPC, true
	}
	return "", false
}
