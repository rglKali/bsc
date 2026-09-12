package api_test

import (
	"testing"

	"bsc/api"
	"bsc/watcher"
)

// The API adds newly derived addresses to the watcher's match set. If these
// signatures ever drift apart the first transfer to a new deposit address would
// be silently missed, so the coupling is asserted at compile time.
var _ api.Addresses = (*watcher.AddrSet)(nil)

// The watcher publishes its lag for the API's staleness check.
var _ api.Sync = (*watcher.Watcher)(nil)

func TestWiringCompiles(t *testing.T) {}
