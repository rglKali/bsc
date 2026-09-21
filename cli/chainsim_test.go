package cli

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sync"
	"testing"

	"bsc/usdt"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var transferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// chainSim is a miniature BSC: it holds balances and allowances, and a
// broadcast transaction actually takes effect and appears in the next block.
//
// A fake that only records calls would prove the packages compile together; one
// that applies transfers proves they agree on what a transfer *means* — which
// is where an integration bug would really live.
type chainSim struct {
	mu sync.Mutex

	token  common.Address
	head   uint64
	blocks map[uint64][]*types.Receipt

	bnb       map[common.Address]*big.Int
	usdt      map[common.Address]*big.Int
	allowance map[common.Address]map[common.Address]*big.Int
	nonces    map[common.Address]uint64

	// pending holds transactions broadcast since the last block was sealed.
	pending []*types.Transaction
	reverts map[common.Hash]bool
}

func newChainSim(token common.Address) *chainSim {
	return &chainSim{
		token:     token,
		blocks:    map[uint64][]*types.Receipt{},
		bnb:       map[common.Address]*big.Int{},
		usdt:      map[common.Address]*big.Int{},
		allowance: map[common.Address]map[common.Address]*big.Int{},
		nonces:    map[common.Address]uint64{},
		reverts:   map[common.Hash]bool{},
	}
}

// bnbOf is an address's native balance, for asserting that something did or did
// not cost gas.
func (c *chainSim) bnbOf(a common.Address) *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.bnb[a]; ok {
		return new(big.Int).Set(v)
	}
	return new(big.Int)
}

// height is the current sealed head, used by tests to count how many blocks a
// piece of work took.
func (c *chainSim) height() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head
}

// --- the watcher's view ---

func (c *chainSim) Finalized(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head, nil
}

func (c *chainSim) BlockReceipts(_ context.Context, n uint64) ([]*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocks[n], nil
}

// --- the sender's view ---

func (c *chainSim) Nonce(_ context.Context, a common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonces[a], nil
}

func (c *chainSim) GasPrice(context.Context) (*big.Int, error) {
	return big.NewInt(3_000_000_000), nil
}

func (c *chainSim) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 50_000, nil
}

func (c *chainSim) BalanceBNB(_ context.Context, a common.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(orZeroInt(c.bnb[a])), nil
}

func (c *chainSim) TokenBalance(_ context.Context, _, holder common.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(orZeroInt(c.usdt[holder])), nil
}

// Call answers balanceOf and allowance, matched by selector.
func (c *chainSim) Call(_ context.Context, _ common.Address, data []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	abi := usdt.NewUsdt()
	var out common.Hash
	switch {
	case bytes.HasPrefix(data, abi.PackBalanceOf(common.Address{})[:4]):
		owner := common.BytesToAddress(data[16:36])
		orZeroInt(c.usdt[owner]).FillBytes(out[:])
	case bytes.HasPrefix(data, abi.PackAllowance(common.Address{}, common.Address{})[:4]):
		owner := common.BytesToAddress(data[16:36])
		spender := common.BytesToAddress(data[48:68])
		if m := c.allowance[owner]; m != nil {
			orZeroInt(m[spender]).FillBytes(out[:])
		}
	}
	return out.Bytes(), nil
}

func (c *chainSim) SendRawTx(_ context.Context, raw []byte) (common.Hash, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pending {
		if p.Hash() == tx.Hash() {
			return tx.Hash(), nil // idempotent re-broadcast, as a node would be
		}
	}
	c.pending = append(c.pending, &tx)
	return tx.Hash(), nil
}

// --- mining ---

// seal applies every pending transaction and publishes them as the next block.
// Applying the effects is what makes the watcher's view agree with the sender's.
func (c *chainSim) seal(t *testing.T) uint64 {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.head++
	block := c.head
	var receipts []*types.Receipt
	logIndex := uint(0)

	for _, tx := range c.pending {
		from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(56)), tx)
		if err != nil {
			t.Fatalf("recover sender: %v", err)
		}
		c.nonces[from] = tx.Nonce() + 1

		rc := &types.Receipt{TxHash: tx.Hash(), Status: types.ReceiptStatusSuccessful, BlockNumber: new(big.Int).SetUint64(block)}
		if c.reverts[tx.Hash()] {
			rc.Status = types.ReceiptStatusFailed
			receipts = append(receipts, rc)
			continue
		}

		abi := usdt.NewUsdt()
		data := tx.Data()
		switch {
		case len(data) == 0 && tx.Value().Sign() > 0: // native BNB transfer
			c.bnb[from] = new(big.Int).Sub(orZeroInt(c.bnb[from]), tx.Value())
			c.bnb[*tx.To()] = new(big.Int).Add(orZeroInt(c.bnb[*tx.To()]), tx.Value())

		case bytes.HasPrefix(data, abi.PackApprove(common.Address{}, big.NewInt(0))[:4]):
			spender := common.BytesToAddress(data[16:36])
			amount := new(big.Int).SetBytes(data[36:68])
			if c.allowance[from] == nil {
				c.allowance[from] = map[common.Address]*big.Int{}
			}
			c.allowance[from][spender] = amount

		case bytes.HasPrefix(data, abi.PackTransferFrom(common.Address{}, common.Address{}, big.NewInt(0))[:4]):
			owner := common.BytesToAddress(data[16:36])
			to := common.BytesToAddress(data[48:68])
			amount := new(big.Int).SetBytes(data[68:100])

			// The master may only move what it has been approved for, and only
			// what is actually there — the same two checks the real token makes.
			allowed := new(big.Int)
			if m := c.allowance[owner]; m != nil {
				allowed = orZeroInt(m[from])
			}
			if allowed.Cmp(amount) < 0 || orZeroInt(c.usdt[owner]).Cmp(amount) < 0 {
				rc.Status = types.ReceiptStatusFailed
				receipts = append(receipts, rc)
				continue
			}
			c.usdt[owner] = new(big.Int).Sub(orZeroInt(c.usdt[owner]), amount)
			c.usdt[to] = new(big.Int).Add(orZeroInt(c.usdt[to]), amount)

			var value common.Hash
			amount.FillBytes(value[:])
			rc.Logs = []*types.Log{{
				Address: c.token,
				Topics: []common.Hash{
					transferTopic,
					common.BytesToHash(owner.Bytes()),
					common.BytesToHash(to.Bytes()),
				},
				Data:  value.Bytes(),
				Index: logIndex,
			}}
			logIndex++
		default:
			t.Fatalf("chainSim: unrecognised calldata %x", data)
		}
		receipts = append(receipts, rc)
	}
	c.pending = nil
	c.blocks[block] = receipts
	return block
}

// deposit simulates an outside party sending USDT to one of our addresses.
func (c *chainSim) deposit(to common.Address, amount *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head++
	c.usdt[to] = new(big.Int).Add(orZeroInt(c.usdt[to]), amount)

	var value common.Hash
	amount.FillBytes(value[:])
	var txHash common.Hash
	txHash[0], txHash[1] = byte(c.head), 0xDE
	c.blocks[c.head] = []*types.Receipt{{
		TxHash: txHash, Status: types.ReceiptStatusSuccessful,
		BlockNumber: new(big.Int).SetUint64(c.head),
		Logs: []*types.Log{{
			Address: c.token,
			Topics: []common.Hash{
				transferTopic,
				common.BytesToHash(common.HexToAddress("0xf0").Bytes()),
				common.BytesToHash(to.Bytes()),
			},
			Data: value.Bytes(),
		}},
	}}
}

func (c *chainSim) fund(a common.Address, v *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bnb[a] = new(big.Int).Add(orZeroInt(c.bnb[a]), v)
}

func (c *chainSim) usdtOf(a common.Address) *big.Int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(orZeroInt(c.usdt[a]))
}

func (c *chainSim) String() string {
	return fmt.Sprintf("head=%d pending=%d", c.head, len(c.pending))
}

func orZeroInt(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
