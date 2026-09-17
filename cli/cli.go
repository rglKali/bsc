// Package cli is bsc's command line: a cobra command tree over the viper
// configuration.
//
// Precedence is the usual one — a flag beats an environment variable beats a
// default — which matters because the service is normally configured by
// systemd's environment while an operator running it by hand wants to override
// one thing without editing a unit file.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"bsc/app"
	"bsc/buildinfo"
	"bsc/config"
	"bsc/store"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// (the version now lives in bsc/buildinfo, stamped at link time)

// Execute runs the command line, returning the process exit code.
func Execute() int {
	// code lets a subcommand report a non-zero result that is not an error:
	// `inspect` finding something is a legitimate outcome, not a failure to run.
	code := 0
	if err := Root(&code).Execute(); err != nil {
		return 1 // cobra has already printed it
	}
	return code
}

// Root builds the command tree, reporting a subcommand's exit code through code.
// Exported so tests can drive it with their own output and arguments.
func Root(code *int) *cobra.Command {
	v := config.New()
	// Set by the --config flag, read in PersistentPreRunE before any command
	// looks at a setting. An empty path means the default location, which is
	// allowed to be absent; a path the operator named must exist.
	configPath := ""

	cmd := &cobra.Command{
		Use:   "bsc",
		Short: "USDT chain gateway",
		Long: "bsc watches finalized BSC blocks, derives wallets, records what lands on\n" +
			"them, forwards the ones configured to forward, and pays out withdrawals.\n\n" +
			"Run with no subcommand to start the service. Settings come from the config\n" +
			"file (see deploy/config.yaml) and BSC_* environment variables; flags below\n" +
			"override both. BSC_MASTER_SECRET is required and belongs only in the\n" +
			"environment — never in the file.",
		SilenceUsage:  true, // a runtime failure is not a usage error
		SilenceErrors: false,
		Args:          cobra.NoArgs,
		// Runs for every subcommand, so `inspect --rpc` reads the same file the
		// service does rather than a second, divergent configuration.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			path, explicit := configPath, configPath != ""
			if !explicit {
				path = config.DefaultPath
			}
			return config.ReadFile(v, path, explicit)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Parse(v)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			setupLogging(v.GetString("log.level"))

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return app.Run(ctx, cfg)
		},
	}

	// Only the settings worth overriding by hand get flags; everything else
	// stays environment-only rather than growing a flag per variable.
	flags := cmd.Flags()
	flags.String("db", "", "path to the database file (db_path)")
	flags.String("http-addr", "", "listen address (http_addr)")
	flags.String("rpc-url", "", "BSC RPC endpoint (chain.rpc_url)")
	flags.Int("rpc-rate-limit", 0, "shared RPC budget in requests per second (chain.rpc_rate_limit)")
	flags.String("snapshot-dir", "", "directory for periodic backups; empty disables them (snapshot.dir)")
	flags.Bool("ui", false, "mount the dashboard at /ui/ — no authentication (ui_enabled)")
	cmd.PersistentFlags().StringVar(&configPath, "config", "",
		"path to the YAML configuration (default "+config.DefaultPath+", which may be absent)")
	cmd.PersistentFlags().String("log-level", "", "debug, info, warn or error (log.level)")

	bind(v, cmd, map[string]string{
		"db":             "db_path",
		"http-addr":      "http_addr",
		"rpc-url":        "chain.rpc_url",
		"rpc-rate-limit": "chain.rpc_rate_limit",
		"snapshot-dir":   "snapshot.dir",
		"ui":             "ui_enabled",
	})
	_ = v.BindPFlag("log.level", cmd.PersistentFlags().Lookup("log-level"))

	cmd.AddCommand(inspectCmd(v, code), versionCmd())
	cmd.AddCommand(masterCmds(v, code)...)
	return cmd
}

// bind wires flags to viper keys, so an unset flag falls through to the
// environment rather than overriding it with a zero value.
func bind(v *viper.Viper, cmd *cobra.Command, keys map[string]string) {
	for flag, key := range keys {
		_ = v.BindPFlag(key, cmd.Flags().Lookup(flag))
	}
}

func inspectCmd(v *viper.Viper, code *int) *cobra.Command {
	var (
		withRPC bool
		quiet   bool
	)
	cmd := &cobra.Command{
		Use:   "inspect <database.db>",
		Short: "Audit a database file offline",
		Long: "Audit a database file, normally a snapshot.\n\n" +
			"bbolt allows a single writer, so the live database cannot be read while the\n" +
			"service holds it — which is why the service snapshots itself and this works\n" +
			"on the copies. Exits non-zero when the audit finds something, so it can be\n" +
			"wired into cron or a health check.\n\n" +
			"Balances are not checked by default: they cannot be recomputed from our own\n" +
			"records, since the deposit log omits sub-threshold dust and holds no debits.\n" +
			"Pass --rpc to compare every balance against the chain.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			setupLogging(v.GetString("log.level"))

			var (
				rep store.Report
				err error
			)
			if withRPC {
				cfg, cerr := config.Parse(v)
				if cerr != nil {
					return fmt.Errorf("config: %w", cerr)
				}
				rep, err = app.VerifyOnChain(cmd.Context(), args[0], cfg.RPCURL, cfg.RPCRateLimit, cfg.Token)
			} else {
				rep, err = app.Verify(args[0])
			}
			if err != nil {
				return err
			}
			*code = report(cmd.OutOrStdout(), args[0], rep, quiet, withRPC)
			return nil
		},
	}
	cmd.Flags().BoolVar(&withRPC, "rpc", false, "also compare every balance against the chain")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print findings only")
	return cmd
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "bsc", buildinfo.Version)
		},
	}
}

// report prints an audit and returns the exit code it implies.
func report(w io.Writer, path string, rep store.Report, quiet, withRPC bool) int {
	if !quiet {
		fmt.Fprintf(w, "%s\n", path)
		fmt.Fprintf(w, "  wallets      %d (%d forwarding)\n", rep.Wallets, rep.Proxies)
		fmt.Fprintf(w, "  flows        %d\n", rep.Flows)
		fmt.Fprintf(w, "  deposits     %d\n", rep.Deposits)
		fmt.Fprintf(w, "  withdrawals  %d\n", rep.Withdrawals)
		fmt.Fprintf(w, "  held         %s base units\n", rep.Held)
		fmt.Fprintf(w, "  committed    %s base units\n", rep.Committed)
	}
	if rep.OK() {
		if !quiet {
			fmt.Fprintln(w, "  audit        clean")
			if !withRPC {
				// Every index was walked both ways and no wallet owes more
				// than our record says it holds. What is still unchecked is
				// that record against the token itself.
				fmt.Fprintln(w, "  custody      not checked against the chain (re-run with --rpc)")
			}
		}
		return 0
	}
	fmt.Fprintf(w, "\n%d finding(s):\n", len(rep.Findings))
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "  %s\n", f)
	}
	return 1
}

// setupLogging installs the structured logger. With no admin API, the log and
// /metrics are how this service explains itself.
func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
