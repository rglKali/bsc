package watcher

import (
	"sync"

	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
)

// AddrSet is the watched-address set, held in memory because every log in
// every block is tested against it — at ~2.2 blocks per second a database
// lookup per log would dominate the whole service.
//
// This is where merging the services pays off most visibly. In v1 the daemon
// kept its own bbolt copy of these addresses, the gateway had to POST each new
// one to it, and both had to re-register at boot. Here the set is derived from
// the wallets table at startup and updated in the same process that creates a
// wallet — no registration hop, no re-sync, and no second copy to drift.
type AddrSet struct {
	mu sync.RWMutex
	m  map[common.Address]uuid.UUID
}

// NewAddrSet returns an empty set.
func NewAddrSet() *AddrSet {
	return &AddrSet{m: make(map[common.Address]uuid.UUID)}
}

// Load replaces the set from the store. Roughly 40 bytes per wallet, so a
// million deposit addresses is tens of megabytes and needs no eviction policy.
func (s *AddrSet) Load(tx *store.Tx) error {
	m := make(map[common.Address]uuid.UUID)
	if err := tx.EachWallet(func(w store.Wallet) error {
		m[w.Address] = w.ID
		return nil
	}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = m
	return nil
}

// Add registers one address. Callers must do this in the same place they create
// the wallet, after the write commits.
func (s *AddrSet) Add(addr common.Address, id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[addr] = id
}

// Lookup resolves an address to its wallet id.
func (s *AddrSet) Lookup(addr common.Address) (uuid.UUID, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.m[addr]
	return id, ok
}

// Len reports how many addresses are watched.
func (s *AddrSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}
