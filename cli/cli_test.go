package cli

import (
	"bsc/buildinfo"
	"bytes"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bsc/config"
	"bsc/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"github.com/spf13/pflag"
)

// run executes the command tree with the given arguments, returning its output
// and exit code.
func run(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	code := 0
	cmd := Root(&code)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), code, err
}

// database writes a small, consistent database and returns its path.
func database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bsc.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w := store.Wallet{
		ID: uuid.New(), Ref: "hot", Kind: store.KindManaged,
		Address: common.HexToAddress("0xabc"),
		Balance: big.NewInt(100), CreatedAt: time.Now(),
	}
	if err := st.Update(func(tx *store.Tx) error { return tx.PutWallet(w) }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Close so the audit can take the read lock: bbolt allows one writer, which
	// is exactly why this tool works on snapshots.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func TestVersion(t *testing.T) {
	// Stamped into bsc/buildinfo at link time rather than passed in, so the
	// reported version cannot disagree with the binary it came from.
	was := buildinfo.Version
	buildinfo.Version = "1.2.3"
	t.Cleanup(func() { buildinfo.Version = was })

	out, code, err := run(t, "version")
	if err != nil || code != 0 {
		t.Fatalf("err=%v code=%d", err, code)
	}
	if !strings.Contains(out, "1.2.3") {
		t.Fatalf("output = %q", out)
	}
}

func TestInspectCleanDatabase(t *testing.T) {
	out, code, err := run(t, "inspect", database(t))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d for a clean database; output:\n%s", code, out)
	}
	if !strings.Contains(out, "audit        clean") {
		t.Fatalf("output = %q", out)
	}
	// A clean offline audit must not be mistaken for a full one.
	if !strings.Contains(out, "custody      not checked") {
		t.Fatalf("output does not say what was skipped:\n%s", out)
	}
}

func TestInspectReportsFindingsAndExitsNonZero(t *testing.T) {
	// Non-zero exit is what makes this usable from cron or a health check.
	path := database(t)
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Update(func(tx *store.Tx) error {
		// A wallet claiming a flow that does not exist: the ownership half of
		// the "one live flow per wallet" invariant, broken.
		var w store.Wallet
		if err := tx.EachWallet(func(got store.Wallet) error {
			w = got
			return nil
		}); err != nil {
			return err
		}
		_, err := tx.MutateWallet(w.ID, func(x *store.Wallet) error {
			x.Flow = uuid.New()
			return nil
		})
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st.Close()

	out, code, err := run(t, "inspect", path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out)
	}
	if !strings.Contains(out, "finding(s)") || !strings.Contains(out, "ownership") {
		t.Fatalf("output = %q", out)
	}
}

func TestInspectQuiet(t *testing.T) {
	out, code, err := run(t, "inspect", "--quiet", database(t))
	if err != nil || code != 0 {
		t.Fatalf("err=%v code=%d", err, code)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("quiet printed %q", out)
	}
}

func TestInspectRequiresAPath(t *testing.T) {
	if _, _, err := run(t, "inspect"); err == nil {
		t.Fatal("inspect accepted no arguments")
	}
	if _, _, err := run(t, "inspect", "a", "b"); err == nil {
		t.Fatal("inspect accepted two arguments")
	}
}

func TestInspectMissingFile(t *testing.T) {
	_, _, err := run(t, "inspect", filepath.Join(t.TempDir(), "absent.db"))
	if err == nil {
		t.Fatal("inspect accepted a missing file")
	}
}

func TestFlagsOverrideTheEnvironment(t *testing.T) {
	// systemd supplies the environment; an operator running it by hand wants to
	// override one thing without editing a unit file. This exercises the same
	// binding Root sets up.
	t.Setenv("BSC_MASTER_SECRET", strings.Repeat("11", 32))
	t.Setenv("BSC_DB_PATH", "/from/env.db")
	t.Setenv("BSC_CHAIN_RPC_RATE_LIMIT", "20")
	t.Setenv("BSC_HTTP_ADDR", "127.0.0.1:9999")

	v := config.New()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("db", "", "")
	fs.Int("rpc-rate-limit", 0, "")
	fs.String("http-addr", "", "")
	if err := fs.Parse([]string{"--db", "/from/flag.db", "--rpc-rate-limit", "40"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for flag, key := range map[string]string{
		"db": "db_path", "rpc-rate-limit": "chain.rpc_rate_limit", "http-addr": "http_addr",
	} {
		if err := v.BindPFlag(key, fs.Lookup(flag)); err != nil {
			t.Fatalf("bind %s: %v", flag, err)
		}
	}

	cfg, err := config.Parse(v)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DBPath != "/from/flag.db" {
		t.Fatalf("db = %q, want the flag to win", cfg.DBPath)
	}
	if cfg.RPCRateLimit != 40 {
		t.Fatalf("rate limit = %d, want the flag to win", cfg.RPCRateLimit)
	}
	// An unset flag must fall through to the environment rather than override
	// it with a zero value.
	if cfg.HTTPAddr != "127.0.0.1:9999" {
		t.Fatalf("http addr = %q, want the environment to survive an unset flag", cfg.HTTPAddr)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	if _, _, err := run(t, "frobnicate"); err == nil {
		t.Fatal("an unknown command was accepted")
	}
}
