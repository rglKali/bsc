# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`bsc/` is a **standalone Go module** (`module bsc`) living inside the dfclub
monorepo. It has **no dependency on `svc/`** and must keep it that way — it is a
reusable USDT chain gateway that happens to be vendored here. Consumers talk to
it over HTTP; nothing imports it.

## Commands (run from `bsc/`)

```sh
task                 # list tasks
task build           # go build ./...
task vet             # go vet ./...
task test            # go test ./...     — no chain, no network, no database server
task cover           # coverage across the service packages
task binary          # -> bin/bsc  (CGO off, -trimpath, version stamped)
GOOS=linux GOARCH=amd64 task binary   # cross-compile for the VPS

task run -- --help            # run the service
task inspect -- snapshot.db   # audit a database file
task abi                      # regenerate usdt/abi.go (needs solc + abigen; do not hand-edit)
```

Single test / package:

```sh
go test ./store/ -run TestVerify -v
go test ./app/ -run TestFullMoneyLifecycle -v   # the end-to-end money path
```

**Every test runs offline.** There is no build tag, no skipped suite, no
`TEST_DATABASE_URL`, and nothing to start first: the store is a temp file, the
chain is a simulator (`app/chainsim_test.go`), and HTTP is `httptest`. Keep it
that way — a test that needs infrastructure will not get run.

## Architecture — one process, one store

```
                    ┌──────────────── bsc (one process) ───────────────┐
 BSC RPC ◀─────────▶│  chain — one dial, one rate limiter, shared      │
                    │     ▲                        ▲                   │
                    │  watcher                  sender                 │
                    │  (finalized blocks)       (signs; holds the key) │
                    │     │                        │                   │
                    │     ▼                        ▼                   │
                    │  ┌────────────────────────────────────────┐      │
 apps (HTTP) ──────▶│  │ store — one bbolt file: state/ data/ log/│     │
 /v1/apps/{slug}/…  │  └────────────────────────────────────────┘      │
                    │     ▲                        ▲                   │
                    │    api                   snapshotter             │
                    └──────────────────────────────────────────────────┘
```

| Package | Role |
| --- | --- |
| `store/` | the only datastore: one bbolt file, packed-binary records, hand-rolled indexes |
| `money/` | the two units and the one conversion between them — cents and wei |
| `keys/` | HMAC key derivation from the master secret |
| `chain/` | the single RPC client (one rate-limit budget for everything) |
| `flow/` | pure pipeline rules: state machines and the work predicates. **No I/O** |
| `engine/` | transactional glue: advance a flow, settle it, evaluate the work rules |
| `watcher/` | follows finalized blocks; one write transaction per block |
| `sender/` | the only code that touches private keys: signs, journals, broadcasts |
| `api/` | the HTTP surface |
| `config/`, `cli/`, `app/` | configuration, the cobra command tree, composition |

### Invariants you must not break

- **Journal before broadcast.** `sender` commits the signed transaction *then*
  sends it, so a crash re-broadcasts the same bytes (same hash, idempotent
  on-chain) rather than signing a second transfer. A transaction that may have
  been broadcast is never abandoned or re-signed — that could double-spend.
- **Signing is strictly sequential.** One transaction in flight at a time, which
  is what keeps a pending-nonce read gapless without a nonce manager.
- **Nonces come from the chain, never a local counter.** The operator holds
  `MASTER_SECRET` and signs by hand for gas top-ups; a stored counter would
  silently desync the moment they did.
- **One write transaction per block.** Confirmations, balances, deposits, newly
  started flows and the cursor commit together. This is what makes a block
  exactly-once with no dedup window.
- **At most one live flow per wallet**, held as `wallet.Flow` — a pointer, not a
  duplicated state. It is what stops a second deposit re-triggering activation.
- **Work is declared, not enqueued** (`flow/rules.go`). Nothing is "scheduled";
  the rules say what *should* exist given current state and converge on it.
  `ShouldDrain` is scoped to `KindDeposit` — an unscoped rule would try to drain
  an app's top-level wallet to itself, forever.
- **`balanceOf` is the authority at signing time**, never our own record. The
  deposit that triggered a drain is only a trigger. This governs what can
  physically move — *not* what an app may spend, which is the ledger.
- **The ledger is the authority on what an app may spend; custody is what a
  wallet holds.** Two quantities, two units, never interchangeable. `App.Ledger`
  is cents and is credited when a drain *lands*; `Wallet.Balance` is wei and is
  whatever the watcher observed. The difference is the house's (§22).
- **Custody is credited for every observed transfer**, including sub-cent dust.
  Only transfers worth a whole cent become deposit records, and only recorded
  deposits ever reach a ledger. A wallet legitimately holds more than its app is
  owed; it must never hold less.
- **Flooring happens exactly once**, in `watcher.credit`, and always rounds
  towards the house. Anywhere else is a bug.
- **Two units, and mixing them is the bug this design fears most.** Cents
  (`money.Cents`, int64) for anything an app can see or spend; wei (`*big.Int`)
  for anything touching the chain. `money.Scale` is the only bridge, and it comes
  from the token's `decimals()` — never a constant.

### Money accounting

An app's balance is an **internal ledger in cents**, recomputable from `log/`:
credited deposits less settled withdrawals. The hot wallet it draws on holds that
plus the house's share, and no arithmetic on the wallet alone can separate them —
which is why the ledger exists (§22).

A withdrawal takes **one** reservation covering payout + fee, because both leave
the ledger at the same moment. Only the payout moves on-chain: the fee is
collected by *not* crediting it, so a withdrawal is one transfer and there is no
`partial` status and no second leg to fail (§24).

Everything nobody is owed — fees, sub-cent remainders, stray transfers — is one
quantity: `custody − ledger`. The **house sweep** collects it, resolving the
amount from `balanceOf` and the ledger at signing time, and refusing outright if
that difference is negative (§25). It is the only operation whose amount is a
computation over somebody else's money; be conservative when touching it.

`bsc inspect` recomputes every ledger from the log, checks `custody ≥ ledger` per
app, and checks both directions of every index. `--rpc` additionally compares our
record of custody against the token. That pair replaces a reconciler process.

## Storage

One bbolt file, three namespaces (`store/keys.go`):

- `state/` — cursor, flows, the in-flight tx index, the send journal. Self-pruning.
- `data/` — apps (carrying their ledgers), wallets, their indexes, plus the token
  identity this database is denominated in. Catastrophic to lose.
- `log/` — deposits and withdrawals. Kept **forever**.

Records are hand-packed binary with a leading version byte: add a field by
appending and bumping the constant, read it under `if v >= N`, never reorder.
Sorted keys do real work — the deposit cursor *is* `<block><logindex>`, the send
journal is nonce-ordered, deposit dedup is a property of the key.

**Secondary indexes are maintained by hand, inside the same transaction as the
record.** That is the cost of having no SQL, and it is where a bug would live.
`store/verify.go` exists to catch exactly that class.

## Wire contract

An app is identified by a **slug in the path**; there is no authentication and no
admin API. The service is loopback-only and every caller is first-party, so a key
would buy nothing but ceremony — and with slug addressing the app API already
*is* the operator's read surface.

**Every amount on the wire is a whole number of cents**, as a decimal string, in
a field named `*_cents`. Never wei, never a decimal point, never a JSON number.
Deposits additionally carry `amount_wei` so an operator can reconcile a record
against a block explorer (§23).

Apps **poll**; nothing is pushed. Deposits are unsolicited so they have a cursor —
the chain's own `(block, log_index)`, which every deposit has by construction.
Withdrawals are app-initiated, so the app polls its own open set by id. Webhooks,
NATS and SSE were all considered and rejected; see `docs/ARCHITECTURE.md`.

## Docs (keep in sync when behavior changes)

- `docs/CONSUMING.md` — the integrator's guide: every endpoint, the cursor, the
  fee model. Update on any API change.
- `docs/ARCHITECTURE.md` — how it works plus a **numbered decision log**. Add an
  entry rather than silently changing a documented decision.
- `docs/OPERATING.md` — day-2 runbook. `docs/ROADMAP.md` — optional backlog.
- `deploy/README.md` — systemd install and the full environment-variable table.

## Gotchas

- **Never edit `deploy/env/*.env.example` or any `.env`** (monorepo-wide rule),
  and never write `MASTER_SECRET` anywhere but a secrets manager. It signs
  everything and holds a `MaxUint256` allowance on every managed wallet, so
  whoever has it can move every app's funds.
- `usdt/abi.go` is generated (`task abi`) — regenerate, don't hand-edit.
- **`MIN_DEPOSIT_WEI` is gone.** `DRAIN_THRESHOLD_WEI` replaced it and means
  something narrower: don't spend gas on a small move. It must never decide
  whether a user is credited — that threshold is one cent, intrinsic, and a
  configurable one could silently confiscate most of a dollar (§23).
- `RPC_RATE_LIMIT` must clear the block rate by a wide margin. At ~0.45s blocks
  the watcher needs ~2.2 req/s just to keep up, and a limit that cannot outrun
  the chain leaves it permanently unable to catch up. Config refuses below 5.
- `START_BLOCK=0` means "the current finalized head", not genesis.
- The watcher must never be starved: it is what observes finality, so anything
  that blocks it stops every in-flight transfer too. Shutdown is decided by
  `ctx.Err()`, never by inspecting an error for `context.DeadlineExceeded`.
- Withdrawals are refused (503) while the watcher is more than `MAX_LAG_BLOCKS`
  behind — reserving against a stale balance could overdraw an app.
