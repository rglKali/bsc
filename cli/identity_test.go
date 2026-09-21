package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
)

// Startup must refuse a database that was built for something else. This is the
// last moment the mistake is cheap: a service that gets past here will read
// blocks perfectly and sign transactions nobody can honour, and under a wrong
// master every wallet in the file is already unreachable (§26, §49).
func TestStartupRefusesADatabaseBuiltForSomethingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bsc.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const (
		chainID  = uint64(56)
		decimals = uint8(18)
	)
	var (
		token  = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
		master = common.HexToAddress("0x1111111111111111111111111111111111111111")
	)

	// First start records the identity; a restart with the same one is normal.
	if err := bindIdentity(st, chainID, token, decimals, master); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := bindIdentity(st, chainID, token, decimals, master); err != nil {
		t.Fatalf("restart with the same identity: %v", err)
	}

	cases := map[string]struct {
		chainID  uint64
		token    common.Address
		decimals uint8
		master   common.Address
		says     string
	}{
		"another chain":    {97, token, decimals, master, "chain"},
		"another token":    {chainID, common.HexToAddress("0x99"), decimals, master, "token"},
		"another decimals": {chainID, token, 6, master, "decimals"},
		"another master": {chainID, token, decimals,
			common.HexToAddress("0x2222222222222222222222222222222222222222"), "strand"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := bindIdentity(st, c.chainID, c.token, c.decimals, c.master)
			if err == nil {
				t.Fatal("started against a database built for something else")
			}
			if !errors.Is(err, store.ErrIdentityMismatch) {
				t.Fatalf("err = %v, want ErrIdentityMismatch", err)
			}
			// The operator reads this in a log line at 3am; it has to say what
			// is wrong and what is at stake, not just that something is.
			if !strings.Contains(err.Error(), "refusing to start") {
				t.Fatalf("err = %v; it should say the service refused to start", err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("err = %v; it should mention %q", err, c.says)
			}
		})
	}

	// And nothing was overwritten on the way through.
	if err := bindIdentity(st, chainID, token, decimals, master); err != nil {
		t.Fatalf("the original identity no longer starts: %v", err)
	}
}
