package swap

import "github.com/ethereum/go-ethereum/common"

// Known routers implementing the Uniswap-V2 interface this package encodes.
//
// These are references, not defaults: swapping stays off until an operator sets
// SWAP_ROUTER deliberately, because it is the only thing the service does on its
// own initiative. Verified against BscScan and PancakeSwap's own announcement,
// September 2026.
//
// Newer venues exist on BNB Smart Chain — PancakeSwap's V3 SmartRouter and
// Infinity Universal Router, and Uniswap v4's Universal Router — and they route
// better. They are deliberately not used here; see docs/ARCHITECTURE.md decision
// 20 for the reasoning, which comes down to a few cents of routing quality not
// being worth Permit2 and command-byte encoding in the one code path whose
// outcome is a price rather than a yes or no.
var (
	// PancakeV2Mainnet is PancakeSwap's V2 router on BNB Smart Chain (56).
	PancakeV2Mainnet = common.HexToAddress("0x10ED43C718714eb63d5aA57B78B54704E256024E")

	// PancakeV2Testnet is the same on Chapel (97).
	PancakeV2Testnet = common.HexToAddress("0xD99D1c33F9fC3444f8101754aBC46c52416550D1")
)
