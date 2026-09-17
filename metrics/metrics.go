// Package metrics holds bsc's Prometheus collectors. Everything lives under a
// single bsc_* namespace on one /metrics endpoint — the v1 split across
// bsc_*, wallet_* and gateway_* went away with the services themselves.
//
// Collectors register on the default registry (which also carries the Go and
// process collectors), so the HTTP layer serves promhttp.Handler() directly.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RPC. One budget is shared by everything that talks to the chain, so these are
// the numbers that say whether that budget is big enough. RPCWait especially:
// at ~2.2 blocks/s the watcher is the heavy user, and a rate limit that cannot
// clear the block rate leaves the service permanently unable to catch up.
var (
	RPCCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_rpc_calls_total",
		Help: "Total JSON-RPC calls issued, by method.",
	}, []string{"method"})

	RPCErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_rpc_errors_total",
		Help: "Total JSON-RPC calls that returned an error, by method.",
	}, []string{"method"})

	RPCDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "bsc_rpc_duration_seconds",
		Help:    "JSON-RPC call latency, excluding time spent waiting on the rate limiter.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	RPCWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "bsc_rpc_wait_seconds",
		Help:    "Time spent waiting on the shared rate limiter before a call could be issued.",
		Buckets: []float64{.001, .005, .01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	})
)

// Chain sync. At ~2.2 blocks per second lag accumulates fast enough that
// BlocksBehind, not the finalized block number, is the gauge to alert on: a
// rate limit that cannot clear the block rate leaves the watcher permanently
// unable to catch up, and this is where that shows.
var (
	FinalizedBlock = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_finalized_block",
		Help: "Highest finalized block reported by the RPC endpoint.",
	})
	CurrentBlock = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_current_block",
		Help: "Highest block fully applied and committed.",
	})
	BlocksBehind = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_blocks_behind",
		Help: "Finalized head minus the block we have committed.",
	})
	BlocksProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bsc_blocks_processed_total",
		Help: "Total blocks applied and committed.",
	})
	CommitDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "bsc_commit_duration_seconds",
		Help:    "Time to apply and commit one write transaction of blocks.",
		Buckets: prometheus.DefBuckets,
	})
	WatchedAddresses = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_watched_addresses",
		Help: "Addresses in the in-memory watch set.",
	})
)

// Money. TransfersSeen counts every USDT transfer touching a watched address;
// DepositsRecorded counts those that became a record. The two differ by the
// transfers that touched the master, which is bsc's own wallet and not a
// deposit anybody is waiting on.
//
// There is no longer an "ignored" series. It counted transfers worth less than
// a cent, which the ledger could not represent; with the chain's own units
// there is no amount too small to record (§36).
var (
	TransfersSeen = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bsc_transfers_seen_total",
		Help: "USDT transfers observed in finalized blocks.",
	})
	DepositsRecorded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bsc_deposits_recorded_total",
		Help: "Incoming transfers recorded as deposits.",
	})
	BalanceUnderflows = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bsc_balance_underflows_total",
		Help: "Debits that would have driven a balance negative. Any value above zero is a bug.",
	})
	MasterBNB = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_master_bnb_wei",
		Help: "Native balance of the master wallet. Inbound native transfers emit no log, so this is polled — and a dry master stops every pipeline.",
	})

	// MasterUSDT is the master's token balance: the fees it has collected, and
	// the balance a gas top-up trades from. Read from the chain rather than from
	// our own record, because the point of a gauge is to show what is actually
	// there — a divergence between the two is what `bsc inspect --rpc` exists
	// to surface.
	MasterUSDT = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_master_usdt_wei",
		Help: "Token balance of the master wallet: collected fees, and what a gas top-up sells.",
	})
)

// Flows.
var (
	FlowsStarted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_flows_started_total",
		Help: "Flows created by the declarative work rules, by kind.",
	}, []string{"kind"})
	FlowTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_flow_transitions_total",
		Help: "Flow state transitions, by kind and resulting state.",
	}, []string{"kind", "state"})
)

// Signing. The sender is strictly sequential — one transaction in flight at a
// time — so InFlight is 0 or 1, and a value pinned at 1 means a transaction is
// wedged rather than that throughput is high.
var (
	TransactionsSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_transactions_sent_total",
		Help: "Transactions signed and broadcast, by action.",
	}, []string{"action"})
	ActionsSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_actions_skipped_total",
		Help: "Actions found unnecessary on-chain and advanced without a transaction, by action.",
	}, []string{"action"})
	BroadcastErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bsc_broadcast_errors_total",
		Help: "Failed eth_sendRawTransaction calls. The journalled transaction is retried, so this is not a loss.",
	})
	Rebroadcasts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "bsc_rebroadcasts_total",
		Help: "Re-sends of an already-journalled transaction that had not landed.",
	})
	InFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_transactions_in_flight",
		Help: "Transactions awaiting finality (0 or 1: signing is sequential).",
	})
	InFlightAge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "bsc_in_flight_age_seconds",
		Help: "Age of the oldest unconfirmed transaction. Growth here is the stuck-transaction signal.",
	})
	InsufficientBalance = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "bsc_insufficient_balance_total",
		Help: "Payments refused at signing time because the chain held less than our records said. Any value above zero means our record of custody drifted.",
	}, []string{"ref"})
)
