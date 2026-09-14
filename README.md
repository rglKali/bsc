# bsc — USDT chain gateway

One service that hands out deposit addresses, collects what lands on them, and
pays out — so the services using it never touch a key, a nonce, or gas.

```
   BSC ──▶ watcher ──┐                        ┌── apps call HTTP, and poll
                     ├──▶ bbolt (one file) ◀──┤
   BSC ◀── sender ───┘                        └── /v1/apps/{slug}/…
```

It is a single process with a single file for state. Everything the service
knows — apps, derived wallets, in-flight transactions, deposits, withdrawals —
lives in one bbolt database, which is what lets a finalized block be applied as
one transaction: confirmations, balances, new work and the chain cursor commit
together or not at all.

Only one component ever touches private keys, and it signs one transaction at a
time.

## Docs

- **[docs/CONSUMING.md](docs/CONSUMING.md)** — the integrator's guide: register an
  app, hand out deposit addresses, pay out, and poll for what happened.
- **[docs/OPERATING.md](docs/OPERATING.md)** — day-2 guide: what to watch, gas,
  backups, recovery.
- **[deploy/README.md](deploy/README.md)** — installing on a VPS under systemd,
  with the full environment-variable table.
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how it works and why, with a
  numbered decision log.
- **[docs/ROADMAP.md](docs/ROADMAP.md)** — optional future work.
- **[e2e/README.md](e2e/README.md)** — the against-a-real-chain suite: what it
  proves that the offline tests cannot, what to fund, and what a run costs.

## Run locally

Needs nothing but a BSC RPC endpoint — no database server, no message broker.

```sh
MASTER_SECRET=<32-byte-hex> task run
curl -X PUT localhost:8800/v1/apps/demo
```

`MASTER_SECRET` is the whole security model: it signs everything and holds an
unlimited allowance on every wallet the service derives. Keep it in a secrets
manager, never in the database and never on disk.

## Build

```sh
task binary                          # -> bin/bsc
GOOS=linux GOARCH=amd64 task binary  # cross-compile for a VPS
task unit                            # offline: no chain, no network, no server
task cover                           # coverage across the service packages
task inspect -- snapshot.db          # audit a database file
```

Every test in `task unit` runs offline — the store is a temp file, the chain is a
simulator, HTTP is `httptest` — so there is nothing to start first and nothing to
skip. The one suite that does spend real funds is `task e2e`, which is
build-tagged and skips unless configured; see
**[e2e/README.md](e2e/README.md)**.

## Design highlights

**Apps get a ledger, not a chain balance.** A hot wallet holds an app's money,
the fees we charged and sub-cent dust in one number, and nothing can make that
number mean "what the app can spend". So balances are an internal ledger in
**cents**, recomputable from the deposit and withdrawal logs — which also makes
`bsc inspect` able to prove the service is solvent, app by app.

**A withdrawal is one transfer, not two.** The business fee leaves the app's
ledger with the payout but never moves on its own: it simply stays in the wallet
as ours. One transaction, no half-settled state to repair by hand, and one rule
later collects everything nobody is owed.

**Nothing is scheduled; work is declared.** Rather than enqueueing a job when a
deposit arrives, the service states what *should* exist — "a deposit wallet
holding more than the drain threshold, with no flow running, is owed a drain" — and
converges on it after every block. Two deposits in one block start one drain; a
deposit landing mid-drain is picked up by the next evaluation rather than lost.

**Crash safety is ordering, not reconciliation.** Every signed transaction is
committed before it is broadcast, so a restart re-sends the same bytes instead of
signing a second transfer. Nothing needs to be reconciled afterwards because
nothing was ever written in two places.

## Layout

```
store/    the only datastore: bbolt, packed binary, hand-rolled indexes
money/    the two units and the one conversion between them: cents and wei
keys/     key derivation from the master secret
chain/    the single RPC client, one shared rate-limit budget
usdt/     generated token bindings (task abi) · swap/ the router calldata
flow/     pure pipeline rules — state machines and work predicates, no I/O
engine/   transactional glue: advance a flow, settle it, evaluate the rules
watcher/  follows finalized blocks; one write transaction per block
sender/   the only code that touches private keys
api/      the HTTP surface · metrics/ the bsc_* namespace
app/      composition · cli/ the command tree · config/ one env config

e2e/      the only tests that spend real funds — build-tagged, skip unless configured
docs/     architecture, operating, consuming, roadmap
deploy/   systemd unit, Prometheus scrape config, Grafana dashboard
```
