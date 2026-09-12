// Package api serves bsc's only HTTP surface.
//
// An app is identified by a slug in the path and there is no authentication.
// The service is loopback-only and every caller is a first-party service on the
// same box, so API keys would buy nothing against that threat model except
// ceremony: a hash table, a rotation endpoint, and a shown-once secret to store
// somewhere. The slug is the same handle callers already configure.
//
// The same reasoning removes the need for an admin API. With slug addressing,
// any local caller can already read any app, so this surface *is* the
// operator's read surface; what is left for operators is /metrics, the logs,
// and `bsc inspect` on a snapshot.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"bsc/keys"
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
	// refused. Reserving against a stale balance could overdraw an app, and the
	// only honest answer while catching up is "not yet".
	MaxLagBlocks uint64

	// DefaultFee is applied to a newly registered app that names no policy.
	DefaultFee store.FeePolicy

	// Notify nudges the sender after work is created. Optional.
	Notify func()
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

// Routes mounts every endpoint. The app is always the first path element, so
// nothing has to be threaded through a middleware.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/apps/{slug}", s.handle(s.putApp))
	mux.HandleFunc("GET /v1/apps/{slug}", s.handle(s.getApp))
	mux.HandleFunc("GET /v1/apps/{slug}/balance", s.handle(s.getBalance))

	mux.HandleFunc("POST /v1/apps/{slug}/wallets", s.handle(s.createDepositAddress))
	mux.HandleFunc("GET /v1/apps/{slug}/wallets", s.handle(s.listDepositAddresses))
	mux.HandleFunc("GET /v1/apps/{slug}/wallets/{ref}", s.handle(s.getDepositAddress))

	mux.HandleFunc("POST /v1/apps/{slug}/withdrawals/quote", s.handle(s.quoteWithdrawal))
	mux.HandleFunc("POST /v1/apps/{slug}/withdrawals", s.handle(s.createWithdrawal))
	mux.HandleFunc("GET /v1/apps/{slug}/withdrawals", s.handle(s.listWithdrawals))
	mux.HandleFunc("GET /v1/apps/{slug}/withdrawals/{id}", s.handle(s.getWithdrawal))

	mux.HandleFunc("GET /v1/apps/{slug}/deposits", s.handle(s.listDeposits))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
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

// app resolves the slug in the path, 404-ing when it is unknown.
func (s *Server) app(tx *store.Tx, r *http.Request) (store.App, error) {
	slug := r.PathValue("slug")
	if err := store.ValidSlug(slug); err != nil {
		return store.App{}, fail(http.StatusBadRequest, "bad_slug", "%v", err)
	}
	a, ok, err := tx.App(slug)
	if err != nil {
		return store.App{}, err
	}
	if !ok {
		return store.App{}, fail(http.StatusNotFound, "unknown_app", "app %q is not registered", slug)
	}
	return a, nil
}

// amount parses a wei-denominated decimal string. Amounts exceed 2⁵³, so they
// are strings on the wire and never JSON numbers.
// cents reads a wire amount. Every amount crossing this API is a whole number
// of cents as a decimal string — "150" is a dollar fifty. Nothing here is ever
// a float, a decimal point, or a wei figure: the chain's unit stops at the edge
// of the service, and an app that never learns it cannot get it wrong (§23).
func cents(field, s string) (money.Cents, error) {
	v, err := money.Parse(s)
	if err != nil {
		return 0, fail(http.StatusBadRequest, "bad_amount",
			"%s must be a whole number of cents, as a decimal string (got %q)", field, s)
	}
	return v, nil
}

func str(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
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
