// Package buildinfo carries build metadata that several parts of the service
// need to report. It has no dependencies, so anything may import it — which is
// the point: the version previously travelled main → cli.SetVersion →
// app.Version → api.Options, and every hop was a chance for one of them to be
// reporting something else.
package buildinfo

// Version is stamped at build time:
//
//	go build -ldflags "-X bsc/buildinfo.Version=$(git describe --tags --always)" .
//
// It is reported by `bsc version`, on the dashboard, and as bsc_build_info, so a
// metric or a support question can be tied to one binary.
var Version = "dev"
