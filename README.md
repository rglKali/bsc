# bsc — USDT chain primitive

One service that derives wallets, tells you what lands on them, forwards the ones
you configure to forward, and pays out on request — so the services using it
never touch a key, a nonce, or gas.

```
   BSC ──▶ watcher ──┐                        ┌── one caller, over HTTP
                     ├──▶ bbolt (one file) ◀──┤
   BSC ◀── sender ───┘                        └── /v1/wallets/…
```

It is a single process with a single file for state. Everything the service
knows — wallets, in-flight transactions, deposits, withdrawals — lives in one
bbolt database, which is what lets a finalized block be applied as one
transaction: confirmations, balances, new work and the chain cursor commit
together or not at all.

Only one component ever touches private keys, and it signs one transaction at a
time.

**bsc keeps no ledger.** It does not know who owes whom, charges no fee, and has
no notion of an app or a user. A service that needs those keeps them above bsc
and asks bsc to move money. That boundary is the whole design — see
`docs/ARCHITECTURE.md` §32–§40 for how it got there, which was partly by
reversing decisions this service had already made.

## Docs

- **[docs/CONSUMING.md](docs/CONSUMING.md)** — the integrator's guide: create a
  wallet, decide whether it forwards, pay out, and poll for what happened.
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
BSC_MASTER_SECRET=<32-byte-hex> task run
curl -X PUT localhost:8800/v1/wallets/treasury -d '{}'
```

`BSC_MASTER_SECRET` is the whole security model: it signs everything and holds an
unlimited allowance on every wallet the service derives. Keep it in a secrets
manager, never in the database and never on disk.

## Build

```sh
task binary                          # -> bin/bsc
GOOS=linux GOARCH=amd64 task binary  # cross-compile for a VPS
task unit                            # offline: no chain, no network, no server
task cover                           # coverage across the service packages
task inspect -- snapshot.db          # audit a database file
task sandbox                         # testnet + the dashboard at /ui/
bsc check                            # the master's balances and the live price
bsc swap --buy-bnb 0.1               # refill gas, by hand, at a price you saw
```

Every test in `task unit` runs offline — the store is a temp file, the chain is a
simulator, HTTP is `httptest` — so there is nothing to start first and nothing to
skip. The one suite that does spend real funds is `task e2e`, which is
build-tagged and skips unless configured; see
**[e2e/README.md](e2e/README.md)**.

## Design highlights

**A wallet either forwards or accumulates.** One nullable `drain_to` is the whole
topology: set, and everything landing on the wallet is swept onward once it is
worth the gas; unset, and it stays there and withdrawals are paid from it. That
replaces a hardcoded two-level shape with a field, which buys chains, fan-in, and
retargeting without a migration — and costs one new check, because a topology you
can set is a topology that can loop.

**There is one unit, and it is the chain's.** Amounts are the token's own base
units, as decimal strings, in every direction. Nothing is scaled and nothing is
rounded, so there is no dust, no minimum, and no remainder belonging to anybody.

**A withdrawal is one transfer, and it cannot fail.** What cannot be honoured is
refused at creation and never becomes a record. What breaks afterwards is ours:
it stays pending, keeps its commitment, and is retried. The caller is never
handed a terminal state it has to compensate for.

**Nothing is scheduled; work is declared.** Rather than enqueueing a job when a
deposit arrives, the service states what *should* exist — "a forwarding wallet
holding more than the drain threshold, with no flow running, is owed a drain" —
and converges on it after every block. Two deposits in one block start one drain;
a deposit landing mid-drain is picked up by the next evaluation rather than lost.

**Crash safety is ordering, not reconciliation.** Every signed transaction is
committed before it is broadcast, so a restart re-sends the same bytes instead of
signing a second transfer. Nothing needs to be reconciled afterwards because
nothing was ever written in two places.

## Layout

```
store/    the only datastore: bbolt, packed binary, hand-rolled indexes
money/    one unit, and the parsing that guards it
keys/     key derivation from the master secret
chain/    the single RPC client, one shared rate-limit budget
usdt/     generated token bindings (task abi) · swap/ the router calldata
flow/     pure pipeline rules — state machines and work predicates, no I/O
engine/   transactional glue: advance a flow, settle it, evaluate the rules
watcher/  follows finalized blocks; one write transaction per block
sender/   the only code that touches private keys
api/      the HTTP surface · metrics/ the bsc_* namespace
ui/       the optional dashboard, embedded; off unless ui_enabled
buildinfo/ the version, stamped at link time
app/      composition · cli/ the command tree · config/ one env config

e2e/      the only tests that spend real funds — build-tagged, skip unless configured
docs/     architecture, operating, consuming, roadmap
deploy/   systemd unit, Prometheus scrape config, Grafana dashboard
```
