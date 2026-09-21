// Package chain is bsc's only connection to BNB Smart Chain: one dial, one rate
// limiter, one set of metrics, shared by everything.
//
// v1 ran two clients against the same provider — a rate-limited read client in
// the daemon and an unlimited write client in the wallet — so nothing owned the
// total. Merging them is the point: the budget is now a single FIFO queue, which
// is not only simpler but safer. Finality is observed by the block watcher, so
// any scheme that let the signing path push the watcher back would mean waiting
// forever for a receipt nobody is fetching. A shared queue makes that
// unrepresentable, and the signing path is a handful of calls behind one
// in-flight transaction, so it cannot meaningfully starve the watcher either.
package chain

import (
	"bsc/usdt"

	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"bsc/metrics"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/time/rate"
)

// finalizedVerifiedValidators is the argument BSC's eth_getFinalizedHeader
// takes. The value is carried over verbatim from the v1 daemon, which ran on it
// in production; confirm it against the BSC RPC documentation before changing
// it, since it governs how strict the finality check is and everything
// downstream treats finalized as irreversible.
const finalizedVerifiedValidators = -3

// Client is the shared RPC client.
type Client struct {
	rpc *rpc.Client
	lim *rate.Limiter
}

// Dial connects to url and applies a shared budget of limit requests per
// second. The initial burst is drained so a restart does not open with a spike
// against the provider.
func Dial(ctx context.Context, url string, limit int) (*Client, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("chain: rate limit must be positive (got %d)", limit)
	}
	cli, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("chain.Dial: %w", err)
	}
	lim := rate.NewLimiter(rate.Limit(limit), limit)
	if err := lim.WaitN(ctx, limit); err != nil {
		cli.Close()
		return nil, fmt.Errorf("chain: drain initial burst: %w", err)
	}
	return &Client{rpc: cli, lim: lim}, nil
}

// Close releases the underlying connection.
func (c *Client) Close() { c.rpc.Close() }

// call is the single path every request takes: wait for budget, time the call,
// count the outcome.
func (c *Client) call(ctx context.Context, result any, method string, args ...any) error {
	start := time.Now()
	if err := c.lim.Wait(ctx); err != nil {
		metrics.RPCErrors.WithLabelValues(method).Inc()
		return fmt.Errorf("chain: rate limiter: %w", err)
	}
	metrics.RPCWait.Observe(time.Since(start).Seconds())

	issued := time.Now()
	err := c.rpc.CallContext(ctx, result, method, args...)
	metrics.RPCDuration.WithLabelValues(method).Observe(time.Since(issued).Seconds())
	metrics.RPCCalls.WithLabelValues(method).Inc()
	if err != nil {
		metrics.RPCErrors.WithLabelValues(method).Inc()
		return fmt.Errorf("chain.%s: %w", method, err)
	}
	return nil
}

// --- reads the watcher needs ---

// Finalized returns the highest finalized block number. Everything bsc does is
// finalized-only, which is what makes reorgs a non-issue rather than a design
// problem.
// ChainID asks the endpoint which chain it is.
//
// bsc reads this rather than being told, because a configured chain id that
// disagreed with the endpoint would be undetectable and total: the signer binds
// every transaction to it (EIP-155), so the node rejects all of them, while the
// watcher happily keeps reading blocks. Deposits would be detected and never
// drained, with nothing in the logs to say why.
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	var raw hexutil.Uint64
	if err := c.call(ctx, &raw, "eth_chainId"); err != nil {
		return 0, fmt.Errorf("chain: read eth_chainId: %w", err)
	}
	if raw == 0 {
		return 0, errors.New("chain: endpoint reported chain id 0")
	}
	return uint64(raw), nil
}

func (c *Client) Finalized(ctx context.Context) (uint64, error) {
	var head types.Header
	if err := c.call(ctx, &head, "eth_getFinalizedHeader", finalizedVerifiedValidators); err != nil {
		return 0, err
	}
	if head.Number == nil {
		return 0, ethereum.NotFound
	}
	return head.Number.Uint64(), nil
}

// BlockReceipts returns every receipt in a block — one call per block, which at
// ~0.45s block times is the dominant cost in the whole RPC budget.
func (c *Client) BlockReceipts(ctx context.Context, block uint64) ([]*types.Receipt, error) {
	var receipts []*types.Receipt
	if err := c.call(ctx, &receipts, "eth_getBlockReceipts", hexutil.Uint64(block).String()); err != nil {
		return nil, err
	}
	if receipts == nil {
		return nil, ethereum.NotFound
	}
	return receipts, nil
}

// --- reads and writes the signing path needs ---

// Receipt returns one transaction's receipt, or nil when it is not yet mined.
//
// The watcher never needs this — it reads whole blocks — but an attended
// an operator does by hand: an approve has to land before the trade that
// builds the trade that depends on it.
func (c *Client) Receipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	var rc *types.Receipt
	if err := c.call(ctx, &rc, "eth_getTransactionReceipt", hash); err != nil {
		return nil, fmt.Errorf("chain: receipt %s: %w", hash.Hex(), err)
	}
	return rc, nil
}

// Nonce returns the next nonce for addr, counting pending transactions. The
// chain is deliberately the authority here rather than a locally persisted
// counter: the operator holds the master secret and signs by hand for gas
// top-ups, and a stored counter would silently desync the moment they did.
func (c *Client) Nonce(ctx context.Context, addr common.Address) (uint64, error) {
	var out hexutil.Uint64
	if err := c.call(ctx, &out, "eth_getTransactionCount", addr, "pending"); err != nil {
		return 0, err
	}
	return uint64(out), nil
}

// GasPrice returns the current suggested gas price in wei.
func (c *Client) GasPrice(ctx context.Context) (*big.Int, error) {
	var out hexutil.Big
	if err := c.call(ctx, &out, "eth_gasPrice"); err != nil {
		return nil, err
	}
	return (*big.Int)(&out), nil
}

// EstimateGas estimates the gas needed to execute msg.
func (c *Client) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	var out hexutil.Uint64
	if err := c.call(ctx, &out, "eth_estimateGas", toCallArg(msg)); err != nil {
		return 0, err
	}
	return uint64(out), nil
}

// Call executes a read-only eth_call, used for ERC-20 views like balanceOf and
// allowance.
func (c *Client) Call(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	var out hexutil.Bytes
	arg := map[string]any{"to": to, "input": hexutil.Bytes(data)}
	if err := c.call(ctx, &out, "eth_call", arg, "latest"); err != nil {
		return nil, err
	}
	return out, nil
}

// BalanceBNB returns the native balance of addr in wei. Inbound native
// transfers emit no log, so this is the only way to see an operator's manual
// gas top-up of the master — the one quantity in the system that cannot be
// derived from logs and our own receipts.
func (c *Client) BalanceBNB(ctx context.Context, addr common.Address) (*big.Int, error) {
	var out hexutil.Big
	if err := c.call(ctx, &out, "eth_getBalance", addr, "latest"); err != nil {
		return nil, err
	}
	return (*big.Int)(&out), nil
}

// TokenBalance reads balanceOf(holder) on a token contract.
//
// The service does not use this for managed wallets — their custody comes from
// the Transfer logs the watcher already applies, with no extra call per wallet.
// It exists for the master, which is not a wallet this service manages and so
// has no record to read (§49).
func (c *Client) TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	out, err := c.Call(ctx, token, usdt.NewUsdt().PackBalanceOf(holder))
	if err != nil {
		return nil, fmt.Errorf("chain: balanceOf(%s) on %s: %w", holder.Hex(), token.Hex(), err)
	}
	return usdt.NewUsdt().UnpackBalanceOf(out)
}

// SendRawTx broadcasts a signed, RLP-encoded transaction. The caller must have
// journalled it first: on restart the same bytes are re-broadcast, which is
// idempotent on-chain, rather than a second transaction being signed.
func (c *Client) SendRawTx(ctx context.Context, raw []byte) (common.Hash, error) {
	var out common.Hash
	if err := c.call(ctx, &out, "eth_sendRawTransaction", hexutil.Encode(raw)); err != nil {
		return common.Hash{}, err
	}
	return out, nil
}

// toCallArg encodes a CallMsg for eth_call and eth_estimateGas.
func toCallArg(msg ethereum.CallMsg) any {
	arg := map[string]any{}
	if msg.From != (common.Address{}) {
		arg["from"] = msg.From
	}
	if msg.To != nil {
		arg["to"] = msg.To
	}
	if len(msg.Data) > 0 {
		arg["input"] = hexutil.Bytes(msg.Data)
	}
	if msg.Value != nil {
		arg["value"] = (*hexutil.Big)(msg.Value)
	}
	if msg.Gas != 0 {
		arg["gas"] = hexutil.Uint64(msg.Gas)
	}
	if msg.GasPrice != nil {
		arg["gasPrice"] = (*hexutil.Big)(msg.GasPrice)
	}
	return arg
}

// TokenDecimals reads a token's decimals().
//
// It is part of IERC20Metadata rather than IERC20, so the generated bindings do
// not carry it and the selector is spelled out here. bsc asks rather than being
// told: the scale between the ledger's cents and the chain's wei follows from
// this number, and a configured value that disagreed with the token would make
// every amount in the service wrong by orders of magnitude while appearing to
// work.
func (c *Client) TokenDecimals(ctx context.Context, token common.Address) (uint8, error) {
	out, err := c.Call(ctx, token, crypto.Keccak256([]byte("decimals()"))[:4])
	if err != nil {
		return 0, fmt.Errorf("chain: read decimals() from %s: %w", token.Hex(), err)
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("chain: %s returned nothing for decimals() — wrong address, or not a token", token.Hex())
	}
	v := new(big.Int).SetBytes(out)
	if !v.IsUint64() || v.Uint64() > 255 {
		return 0, fmt.Errorf("chain: %s reports an implausible decimals() of %s", token.Hex(), v)
	}
	return uint8(v.Uint64()), nil
}
