package chain

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// rpcServer is an in-process JSON-RPC endpoint: canned results per method, with
// the requests recorded so tests can assert what was actually sent. Keeping it
// in-process is what lets the whole suite run with no chain and no network.
type rpcServer struct {
	*httptest.Server

	mu       sync.Mutex
	results  map[string]json.RawMessage
	failWith map[string]string
	requests []rpcRequest
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params []any           `json:"params"`
}

func newRPCServer(t *testing.T) *rpcServer {
	t.Helper()
	s := &rpcServer{
		results:  map[string]json.RawMessage{},
		failWith: map[string]string{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *rpcServer) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	result, ok := s.results[req.Method]
	failure, failing := s.failWith[req.Method]
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case failing:
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32000, "message": failure},
		})
	case !ok:
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": "method not stubbed: " + req.Method},
		})
	default:
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"jsonrpc": "2.0", "id": req.ID, "result": result,
		})
	}
}

// stub registers a canned result, marshalling it the same way a node would.
func (s *rpcServer) stub(t *testing.T, method string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal stub for %s: %v", method, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[method] = raw
}

func (s *rpcServer) fail(method, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith[method] = message
}

// lastParams returns the params of the most recent call to method.
func (s *rpcServer) lastParams(t *testing.T, method string) []any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if s.requests[i].Method == method {
			return s.requests[i].Params
		}
	}
	t.Fatalf("no call to %s was made", method)
	return nil
}

// dial connects to the fake with a limit high enough not to pace the tests.
func dial(t *testing.T, s *rpcServer) *Client {
	t.Helper()
	c, err := Dial(context.Background(), s.URL, 10_000)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestDialRejectsNonPositiveLimit(t *testing.T) {
	s := newRPCServer(t)
	for _, limit := range []int{0, -1} {
		if _, err := Dial(context.Background(), s.URL, limit); err == nil {
			t.Fatalf("Dial accepted limit %d", limit)
		}
	}
}

func TestFinalized(t *testing.T) {
	s := newRPCServer(t)
	s.stub(t, "eth_getFinalizedHeader", &types.Header{
		Number:     big.NewInt(48210577),
		Difficulty: big.NewInt(1),
		GasLimit:   1,
		GasUsed:    1,
		Time:       1,
		Extra:      []byte{},
	})
	c := dial(t, s)

	got, err := c.Finalized(context.Background())
	if err != nil {
		t.Fatalf("Finalized: %v", err)
	}
	if got != 48210577 {
		t.Fatalf("Finalized = %d", got)
	}
	// The finality argument is chain-facing and carried over from v1 verbatim;
	// pin it so it cannot drift silently.
	params := s.lastParams(t, "eth_getFinalizedHeader")
	if len(params) != 1 {
		t.Fatalf("params = %v", params)
	}
	if n, ok := params[0].(float64); !ok || int(n) != finalizedVerifiedValidators {
		t.Fatalf("finality argument = %v, want %d", params[0], finalizedVerifiedValidators)
	}
}

func TestFinalizedWithoutANumberIsNotFound(t *testing.T) {
	s := newRPCServer(t)
	s.stub(t, "eth_getFinalizedHeader", map[string]any{})
	c := dial(t, s)

	if _, err := c.Finalized(context.Background()); err == nil {
		t.Fatal("Finalized accepted a header with no number")
	}
}

func TestBlockReceipts(t *testing.T) {
	s := newRPCServer(t)
	want := []*types.Receipt{{
		Type:              types.LegacyTxType,
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 21000,
		GasUsed:           21000,
		EffectiveGasPrice: big.NewInt(3_000_000_000),
		TxHash:            common.HexToHash("0xaa"),
		BlockNumber:       big.NewInt(100),
		Logs:              []*types.Log{},
	}}
	s.stub(t, "eth_getBlockReceipts", want)
	c := dial(t, s)

	got, err := c.BlockReceipts(context.Background(), 100)
	if err != nil {
		t.Fatalf("BlockReceipts: %v", err)
	}
	if len(got) != 1 || got[0].TxHash != want[0].TxHash || got[0].Status != types.ReceiptStatusSuccessful {
		t.Fatalf("got %+v", got)
	}
	// The block must go out as hex, not decimal.
	if p := s.lastParams(t, "eth_getBlockReceipts"); len(p) != 1 || p[0] != "0x64" {
		t.Fatalf("params = %v, want [0x64]", p)
	}
}

func TestBlockReceiptsNullIsNotFound(t *testing.T) {
	// A node that does not have the block answers null rather than erroring;
	// treating that as an empty block would silently skip its deposits.
	s := newRPCServer(t)
	s.stub(t, "eth_getBlockReceipts", nil)
	c := dial(t, s)

	_, err := c.BlockReceipts(context.Background(), 100)
	if err == nil || !strings.Contains(err.Error(), ethereum.NotFound.Error()) {
		t.Fatalf("got %v, want not found", err)
	}
}

func TestNonceUsesPendingState(t *testing.T) {
	// A "latest" nonce would collide with a transaction we have already
	// broadcast but that has not yet been mined.
	s := newRPCServer(t)
	s.stub(t, "eth_getTransactionCount", "0x2a")
	c := dial(t, s)

	got, err := c.Nonce(context.Background(), common.HexToAddress("0x01"))
	if err != nil {
		t.Fatalf("Nonce: %v", err)
	}
	if got != 42 {
		t.Fatalf("Nonce = %d, want 42", got)
	}
	p := s.lastParams(t, "eth_getTransactionCount")
	if len(p) != 2 || p[1] != "pending" {
		t.Fatalf("params = %v, want the pending block tag", p)
	}
}

func TestGasPriceAndBalance(t *testing.T) {
	s := newRPCServer(t)
	s.stub(t, "eth_gasPrice", "0xb2d05e00") // 3 gwei
	s.stub(t, "eth_getBalance", "0xde0b6b3a7640000")
	c := dial(t, s)

	price, err := c.GasPrice(context.Background())
	if err != nil {
		t.Fatalf("GasPrice: %v", err)
	}
	if price.Cmp(big.NewInt(3_000_000_000)) != 0 {
		t.Fatalf("GasPrice = %s", price)
	}

	bal, err := c.BalanceBNB(context.Background(), common.HexToAddress("0x01"))
	if err != nil {
		t.Fatalf("BalanceBNB: %v", err)
	}
	want, _ := new(big.Int).SetString("1000000000000000000", 10)
	if bal.Cmp(want) != 0 {
		t.Fatalf("BalanceBNB = %s, want %s", bal, want)
	}
}

func TestEstimateGasEncodesTheMessage(t *testing.T) {
	s := newRPCServer(t)
	s.stub(t, "eth_estimateGas", "0xcf08") // 53000
	c := dial(t, s)

	from, to := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	got, err := c.EstimateGas(context.Background(), ethereum.CallMsg{
		From: from, To: &to, Data: []byte{0x09, 0x5e, 0xa7, 0xb3}, Value: big.NewInt(7),
	})
	if err != nil {
		t.Fatalf("EstimateGas: %v", err)
	}
	if got != 53000 {
		t.Fatalf("EstimateGas = %d", got)
	}
	p := s.lastParams(t, "eth_estimateGas")
	arg, ok := p[0].(map[string]any)
	if !ok {
		t.Fatalf("params = %v", p)
	}
	for _, key := range []string{"from", "to", "input", "value"} {
		if _, ok := arg[key]; !ok {
			t.Fatalf("call arg missing %q: %v", key, arg)
		}
	}
	// An empty field must be omitted rather than sent as zero, which some nodes
	// reject outright.
	if _, ok := arg["gas"]; ok {
		t.Fatalf("unset gas was sent: %v", arg)
	}
}

func TestCallUsesLatestState(t *testing.T) {
	s := newRPCServer(t)
	s.stub(t, "eth_call", "0x0000000000000000000000000000000000000000000000000de0b6b3a7640000")
	c := dial(t, s)

	out, err := c.Call(context.Background(), common.HexToAddress("0x55d3"), []byte{0x70, 0xa0, 0x82, 0x31})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(out) != 32 {
		t.Fatalf("Call returned %d bytes", len(out))
	}
	if got := new(big.Int).SetBytes(out); got.Cmp(big.NewInt(1e18)) != 0 {
		t.Fatalf("decoded %s", got)
	}
	if p := s.lastParams(t, "eth_call"); len(p) != 2 || p[1] != "latest" {
		t.Fatalf("params = %v, want the latest block tag", p)
	}
}

func TestSendRawTx(t *testing.T) {
	s := newRPCServer(t)
	s.stub(t, "eth_sendRawTransaction", common.HexToHash("0xbeef"))
	c := dial(t, s)

	got, err := c.SendRawTx(context.Background(), []byte{0xf8, 0x6c})
	if err != nil {
		t.Fatalf("SendRawTx: %v", err)
	}
	if got != common.HexToHash("0xbeef") {
		t.Fatalf("SendRawTx = %s", got.Hex())
	}
	if p := s.lastParams(t, "eth_sendRawTransaction"); len(p) != 1 || p[0] != "0xf86c" {
		t.Fatalf("params = %v, want hex-encoded raw bytes", p)
	}
}

func TestErrorsNameTheMethod(t *testing.T) {
	// These strings end up in logs during an incident, so the method has to be
	// in them — "execution reverted" alone says nothing about what was called.
	s := newRPCServer(t)
	s.fail("eth_gasPrice", "upstream exploded")
	c := dial(t, s)

	_, err := c.GasPrice(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "eth_gasPrice") || !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("error = %v", err)
	}
}

func TestRateLimiterPacesAndRespectsContext(t *testing.T) {
	// One shared budget for everything: proof that it actually gates, and that
	// waiting for it honours cancellation rather than blocking a caller
	// indefinitely.
	s := newRPCServer(t)
	s.stub(t, "eth_gasPrice", "0x1")
	c, err := Dial(context.Background(), s.URL, 1) // 1 req/s, burst drained
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GasPrice(ctx); err == nil {
		t.Fatal("a call beyond the budget succeeded within the deadline")
	} else if !strings.Contains(err.Error(), "rate limiter") {
		t.Fatalf("error = %v, want a rate-limiter failure", err)
	}

	// With enough time the same call goes through.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := c.GasPrice(ctx2); err != nil {
		t.Fatalf("GasPrice once budget allowed: %v", err)
	}
}
