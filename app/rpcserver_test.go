package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rpcServerFor exposes a chainSim over JSON-RPC so the real chain client can
// talk to it, which is what lets VerifyOnChain be tested without a node.
func rpcServerFor(t *testing.T, sim *chainSim) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		if req.Method == "eth_call" {
			arg, _ := req.Params[0].(map[string]any)
			data, _ := arg["input"].(string)
			out, err := sim.Call(r.Context(), sim.token, hexBytes(data))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			result = "0x" + hexString(out)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func hexBytes(s string) []byte {
	s = trim0x(s)
	out := make([]byte, len(s)/2)
	for i := range out {
		out[i] = fromHex(s[2*i])<<4 | fromHex(s[2*i+1])
	}
	return out
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

func trim0x(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}

func fromHex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
