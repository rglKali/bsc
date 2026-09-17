// Command bsc is a USDT chain primitive: one process that watches finalized BSC
// blocks, derives wallets, forwards the ones configured to forward, and pays out
// withdrawals on request. See docs/ARCHITECTURE.md for the design.
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
