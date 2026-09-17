// Package api serves bsc's only HTTP surface.
//
// The noun is a wallet. A caller asks for one, is told what arrived on it, and
// asks for a payout from it — and that is the whole vocabulary. There is no app,
// no balance split, no fee and no quote, because none of those are questions
// about a chain: they are questions about who owes whom, and the service that
// keeps those books sits above this one (§33).
//
// There is no authentication. bsc is loopback-only and its caller is a
// first-party service on the same box, so API keys would buy nothing against
// that threat model except ceremony. That also means there is no tenancy here:
// any local caller can read every wallet, and keeping one caller's handles from
// colliding with another's is the caller's job (§37).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"bsc/keys"
	"math/big"

	"bsc/money"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// Sync reports how far the chain watcher is behind the finalized head.
type Sync interface {
	Behind() uint64
}

// Addresses is the watcher's in-memory match set. A newly derived deposit
// address must be added to it in the same place it is created, or the first
// transfer to it would be missed.
type Addresses interface {
	Add(addr common.Address, id uuid.UUID)
}

// Options configure the server.
type Options struct {
	// MaxLagBlocks is how far behind the watcher may be before withdrawals are
	// refused. Accepting one against a stale balance could overdraw a wallet,
	// and the only honest answer while catching up is "not yet".
	MaxLagBlocks uint64

	// Notify nudges the sender after work is created. Optional.
	Notify func()

	// UI mounts the local dashboard at /ui/ along with its read model. Off
	// unless the operator asked for it: this listener has no authentication
	// (§29).
	UI bool

	// Health supplies what /healthz cannot derive for itself. The zero value
	// simply makes the gas check a no-op.
	Health Health
}

// Server holds the handlers' dependencies.
type Server struct {
	store *store.Store
	ring  *keys.Ring
	addrs Addresses
	sync  Sync
	opts  Options
	log   *slog.Logger
}

// New builds the server.
func New(st *store.Store, ring *keys.Ring, addrs Addresses, sync Sync, opts Options) *Server {
	if opts.MaxLagBlocks == 0 {
		opts.MaxLagBlocks = 200 // ~90s at 0.45s blocks
	}
	if opts.Notify == nil {
		opts.Notify = func() {}
	}
	if sync == nil {
		// Fail closed. A missing sync source means we cannot tell whether
		// balances are current, and refusing every withdrawal is the safe
		// reading of that — silently treating it as "caught up" would disable a
		// guard that exists to stop an app overdrawing.
		sync = alwaysStale{}
	}
	return &Server{
		store: st, ring: ring, addrs: addrs, sync: sync, opts: opts,
		log: slog.Default().With("svc", "api"),
	}
}

// Routes mounts every endpoint.
//
// A wallet is addressed by the ref its caller chose, and everything about that
// wallet hangs off its own path: `PUT` to create it, `GET` to read it, `PATCH`
// to reconfigure it, and its credits and debits underneath. The two collections
// that are not about one wallet — the global feeds — sit at the top level.
//
// `PUT` rather than `POST` for creation because that is what it does: the ref
// is the identifier, the caller supplies it, and calling twice is the same as
// calling once. A `POST` to a collection would imply the server picks the name.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/wallets", s.handle(s.listWallets))
	mux.HandleFunc("PUT /v1/wallets/{ref}", s.handle(s.putWallet))
	mux.HandleFunc("GET /v1/wallets/{ref}", s.handle(s.getWallet))
	mux.HandleFunc("PATCH /v1/wallets/{ref}", s.handle(s.patchWallet))

	mux.HandleFunc("GET /v1/wallets/{ref}/deposits", s.handle(s.listWalletDeposits))
	mux.HandleFunc("POST /v1/wallets/{ref}/withdrawals", s.handle(s.createWithdrawal))
	mux.HandleFunc("GET /v1/wallets/{ref}/withdrawals", s.handle(s.listWalletWithdrawals))

	// The two feeds. Both are observations of transfers that have settled, both
	// are cursor-ordered, and a caller reconciles a wallet by walking them
	// together — which is the whole reason a drain is a debit rather than a
	// third kind of thing (§42).
	mux.HandleFunc("GET /v1/deposits", s.handle(s.listDeposits))
	mux.HandleFunc("GET /v1/withdrawals", s.handle(s.listWithdrawals))
	mux.HandleFunc("GET /v1/withdrawals/{id}", s.handle(s.getWithdrawal))

	// The dashboard and its read model, when the operator asked for them. They
	// are mounted last and live outside /v1 entirely, so the caller contract is
	// unchanged whether this is on or off (§29).
	if s.opts.UI {
		s.mountUI(mux)
	}

	mux.HandleFunc("GET /healthz", s.handle(s.getHealth))
}

// --- errors ---

// apiError carries an HTTP status alongside the message. Handlers return these
// so the mapping from a domain failure to a status code lives in one place.
type apiError struct {
	status int
	code   string
	err    error
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

func fail(status int, code string, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, err: fmt.Errorf(format, args...)}
}

var errNotFound = fail(http.StatusNotFound, "not_found", "not found")

// handle wraps a handler with JSON error rendering. A domain error that has not
// been given a status is a 500 and is logged: callers get a generic message,
// because an unexpected error's text is for us, not for them.
func (s *Server) handle(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r)
		if err == nil {
			return
		}
		var ae *apiError
		if errors.As(err, &ae) {
			writeJSON(w, ae.status, map[string]string{"error": ae.Error(), "code": ae.code})
			return
		}
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal error", "code": "internal",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func decode(r *http.Request, into any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // a misspelled field must not be silently ignored
	if err := dec.Decode(into); err != nil {
		return fail(http.StatusBadRequest, "bad_request", "invalid JSON body: %v", err)
	}
	return nil
}

// --- shared helpers ---

// wallet resolves the ref in the path, 404-ing when it is unknown.
func (s *Server) wallet(tx *store.Tx, r *http.Request) (store.Wallet, error) {
	ref := r.PathValue("ref")
	if err := store.ValidRef(ref); err != nil {
		return store.Wallet{}, fail(http.StatusBadRequest, "bad_ref", "%v", err)
	}
	w, ok, err := tx.WalletByRef(ref)
	if err != nil {
		return store.Wallet{}, err
	}
	if !ok {
		return store.Wallet{}, fail(http.StatusNotFound, "unknown_wallet", "no wallet with ref %q", ref)
	}
	return w, nil
}

// amount reads a wire amount: the token's own base units as a decimal string.
//
// There is one unit now and it is the chain's. An amount here is the same
// integer the token moves, so nothing is scaled, nothing is floored, and a
// caller that wants dollars does that conversion in its own books where it
// knows the exchange rate it means (§36).
func amount(field, s string) (*big.Int, error) {
	v, err := money.ParsePositive(s)
	if err != nil {
		return nil, fail(http.StatusBadRequest, "bad_amount",
			"%s must be a positive whole number of base units, as a decimal string (got %q)", field, s)
	}
	return v, nil
}

// address parses and rejects the one destination a valid address can be while
// still being unpayable. The token reverts on the zero address, so a payout to
// it could never land however many times it was retried — and since a
// withdrawal has no failure state, anything permanently unpayable has to be
// caught here rather than settled into one afterwards (§28).
func address(field, raw string) (common.Address, error) {
	if !common.IsHexAddress(raw) {
		return common.Address{}, fail(http.StatusBadRequest, "bad_"+field, "%s must be a hex address", field)
	}
	addr := common.HexToAddress(raw)
	if addr == (common.Address{}) {
		return common.Address{}, fail(http.StatusBadRequest, "bad_"+field,
			"%s is the zero address, which cannot receive tokens", field)
	}
	return addr, nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func hashStr(h common.Hash) string {
	if h == (common.Hash{}) {
		return ""
	}
	return h.Hex()
}

// The watcher's address set must satisfy Addresses; asserted where both are in
// scope so a signature drift is a compile error rather than a missed deposit.

// alwaysStale stands in for a missing Sync, reporting a lag nothing can satisfy.
type alwaysStale struct{}

func (alwaysStale) Behind() uint64 { return ^uint64(0) }

// isErr is errors.Is, kept short because the error mapping reads better as a
// switch of one-line cases.
func isErr(err, target error) bool { return errors.Is(err, target) }
