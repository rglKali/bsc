// Command bsc is the USDT chain gateway: one process that watches finalized BSC
// blocks, derives and drains deposit wallets, and pays out withdrawals for the
// apps registered with it. See docs/REWRITE.md for the design.
package main

import (
	"os"

	"bsc/cli"
)

// The version is stamped straight into bsc/buildinfo at link time (see the
// Taskfile), so nothing has to be threaded through here.
func main() {
	os.Exit(cli.Execute())
}
