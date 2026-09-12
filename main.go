// Command bsc is the USDT chain gateway: one process that watches finalized BSC
// blocks, derives and drains deposit wallets, and pays out withdrawals for the
// apps registered with it. See docs/REWRITE.md for the design.
package main

import (
	"os"

	"bsc/cli"
)

// version is stamped at build time (see the Taskfile).
var version = "dev"

func main() {
	cli.SetVersion(version)
	os.Exit(cli.Execute())
}
