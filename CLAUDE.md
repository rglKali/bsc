# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This is a **standalone Go module** (`module bsc`) in its own repository. It is a
reusable USDT chain primitive with no dependency on any consumer, and must keep it
that way — the internal tools and infrastructure components that use it talk to
it over HTTP; nothing imports it.

## Commands (run from the repository root)

```sh
task                 # list tasks
task build           # go build ./...
task vet             # go vet ./...
task unit            # go test ./...     — no chain, no network, no database server
task cover           # coverage across the service packages
task binary          # -> bin/bsc  (CGO off, -trimpath, version stamped)
GOOS=linux GOARCH=amd64 task binary   # cross-compile for the VPS

task run -- --help            # run the service
task inspect -- snapshot.db   # audit a database file
task abi                      # regenerate usdt/abi.go (needs solc + abigen; do not hand-edit)

bsc check                     # master balances and the gas floor
```

Single test / package:

```sh
go test ./store/ -run TestVerify -v
go test ./cli/ -run TestFullMoneyLifecycle -v   # the end-to-end money path
```

**Every test runs offline.** There is no build tag, no skipped suite, no
`TEST_DATABASE_URL`, and nothing to start first: the store is a temp file, the
chain is a simulator (`cli/chainsim_test.go`), and HTTP is `httptest`. Keep it
that way — a test that needs infrastructure will not get run.

## What this service is

**bsc is a low-level primitive, not a payments provider.** It derives wallets,
records what lands on them, forwards the ones configured to forward, and executes
payouts on request. It keeps **no ledger**, charges **no fee**, and has no notion
of an app, a user or a tenant.

v2.0 had all three. `docs/ARCHITECTURE.md` §32–§40 records why they were removed
and what the boundary is now: whatever keeps the books does so *above* bsc and
asks bsc to move money. Before adding anything that needs to know whose money is
where, read §33 — that is the decision being reopened.

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
 caller (HTTP) ────▶│  │ store — one bbolt file: state/ data/ log/│     │
 /v1/wallets/…      │  └────────────────────────────────────────┘      │
                    │     ▲                        ▲                   │
                    │    api                   snapshotter             │
                    └──────────────────────────────────────────────────┘
```

| Package | Role |
| --- | --- |
| `store/` | the only datastore: one bbolt file, packed-binary records, hand-rolled indexes |
| `keys/` | HMAC key derivation from the master secret, indexed by a wallet's sequential id |
| `chain/` | the single RPC client (one rate-limit budget for everything) |
| `flow/` | the pipeline. `flow.go`/`rules.go` are pure — state machines and work predicates, no I/O; `engine.go` is the transactional half that advances and settles them inside a store transaction |
| `watcher/` | follows finalized blocks; one write transaction per block |
| `sender/` | the only code that touches private keys: signs, journals, broadcasts |
| `usdt/` | generated token bindings (`task abi`) |
| `api/` | the HTTP surface, wire amounts, and the embedded dashboard (off by default, §29) |
| `metrics/` | the one `bsc_*` namespace, served from the same listener |
| `buildinfo/` | the version, stamped at link time; no dependencies |
| `config/` | one configuration, from YAML and the environment |
| `cli/` | the cobra command tree, and the wiring that runs the service (`app.go`) |

### Invariants you must not break

- **Journal before broadcast.** `sender` commits the signed transaction *then*
  sends it, so a crash re-broadcasts the same bytes (same hash, idempotent
  on-chain) rather than signing a second transfer. A transaction that may have
  been broadcast is never abandoned or re-signed — that could double-spend.
- **Signing is strictly sequential.** One transaction in flight at a time, which
  is what keeps a pending-nonce read gapless without a nonce manager.
- **Nonces come from the chain, never a local counter.** The operator signs by
  hand from the master; a stored counter would silently desync the moment they did.
- **One write transaction per block.** Confirmations, balances, deposits, newly
  started flows and the cursor commit together. This is what makes a block
  exactly-once with no dedup window.
- **At most one live flow per wallet**, held as `wallet.Flow` — a pointer, not a
  duplicated state. It is what stops a second deposit re-triggering activation.
- **Work is declared, not enqueued** (`flow/rules.go`). Nothing is "scheduled";
  the rules say what *should* exist given current state and converge on it.
- **One transfer flow, two shapes** (§46). `Amount` and `Withdrawal` travel
  together or not at all — `flow.Begin` enforces it, and settlement reads
  `f.Pays()` rather than a kind. A drain's resolved amount must reach the flow
  before settlement or its debit records zero; `move` returns it and
  `journalAndSend` persists it. Do not split this back into two kinds.
- **`drain_to` is the topology, and forwarding excludes paying out.** `ShouldDrain`
  requires one; `ShouldPay` requires its absence. They are mutually exclusive by
  construction (§32) — do not add a case where a wallet does both, because the
  drain and the payout would race for the same funds.
- **A `drain_to` that loops must be refused** (§35). `A → B → A` succeeds on every
  hop and burns the master's gas forever, with no error anywhere. `CheckDrainChain`
  guards writes; `Verify` re-checks offline. Never bypass it.
- **`balanceOf` is the authority at signing time**, never our own record. The
  deposit that triggered a drain is only a trigger.
- **One unit, everywhere: the token's own base units** (`*big.Int`). There is no
  second unit and no scaling. Nothing is floored, so there is no dust — if you
  find yourself dividing an amount, stop and read §36.
- **Custody is credited for every observed transfer**, with no threshold. Every
  transfer to a wallet in the address set becomes a deposit record. The master
  is not in that set (§49), so nothing it receives is recorded at all.
- **Every movement is a credit or a debit, and nothing else** (§42). A drain is
  a debit with `reason: drain`. Do not add a third kind of record —
  `sum(credits) − sum(debits) == balanceOf` is the property that makes the store
  reconcilable, and it holds only while that is true.
- **A promise and a fact live in different keyspaces** (§51). A pending
  withdrawal is a `store.Pending` in `state/withdrawal`; settling *moves* it to
  `log/withdrawal` as a `store.Withdrawal`. **The bucket is the status** — there
  is no stored field, and there must not be one. A promise carries no `TxHash`
  and a fact carries no `Attempts`. Three lookups pay for this by trying both
  buckets (by id, by idempotency key, a wallet's full history); everything else
  got narrower. `Verify` flags a record found in both.
- **A deposit is an observation, not a lifecycle** (§50). It has no status and
  points at nothing: it is recorded after the fact and never touched again. Do
  not link credits to the debit that swept them — a drain moves a *balance*,
  resolved from `balanceOf` at signing, so any such link is a guess. The two
  ends of a forward pair through the drain debit's `tx_hash`.
- **A debit must always name a reason.** `PutWithdrawal` refuses a zero one
  rather than defaulting it: a record that does not say why money left is the
  bug, not the default.
- **These are observations, never assertions of ownership.** The moment something
  computes a net position *per person* from credits and debits, the ledger is
  back and §33 is undone.
- **A wallet's id is its derivation index, and the sequence only moves forward**
  (§48). `priv = HMAC(master, be64(id))`. The next id is the highest row plus
  one — there is deliberately **no stored counter**, so nothing can drift from
  the table. Ids start at 1 and are not contiguous (`keys.Allocate` skips
  invalid scalars); they are never a position in creation order. Zero is not a
  wallet: `keys.Derive` and `PutWallet` both refuse it.
- **The master is not a wallet** (§49). No row, no `kind`, not in the
  `AddrSet` — its address lives in `meta` and nothing else about it is stored.
  It is the operator's own key, spent from scripts and by hand, and bsc records
  nothing it receives. Every row in `data/wallet` is derived and nameable; do
  not reintroduce a wallet that is an exception to that.
- **The master address is bound like the chain and the token** (§26, §49).
  `SetMeta` refuses a different one, because every address in the file derives
  from that secret and opening it under another strands all of them. Treat
  `BSC_MASTER_SECRET` as permanent: rotating it rotates the whole keyset.
- **Restoring an old snapshot rewinds the id sequence** (§48). That reissues
  indices to different refs — one address, two callers, money merged silently.
  `PutWallet` refuses it while the address record survives; after a restore it
  may not. There is no gap scan. Do not treat a restore as routine.
- **Creating a wallet must never spend gas** (§41). Deriving is HMAC plus a
  bbolt write; activation is the funding/approving prefix of the first flow that
  needs to move money. `prewarm: true` is the explicit opt-out and must stay
  opt-in — a caller with a large address book would otherwise pay two
  transactions per user who never deposits.

### Money

There is one quantity: **custody**, what the chain says an address holds,
accumulated from observed `Transfer` logs.

The overdraft guard is a comparison, not a stored balance (§39):

```
committed  = Σ pending withdrawals on this wallet
available  = balance − committed
accept a withdrawal iff  committed + amount ≤ balance
```

Both sides are records that already exist, so there is nothing to materialise and
nothing that can drift. A reverted payout stays `pending`, so it stays committed —
which is exactly right, and needs no special case.

`bsc inspect` walks every index from both sides — record to entry and entry
back to record (§54) — checks flow/wallet ownership
both ways, checks that no drain chain loops, and checks that no wallet has
promised more than it holds. `--rpc` additionally compares our record of custody
against the token.

## Storage

One bbolt file, three namespaces (`store/keys.go`):

- `state/` — cursor, flows, the in-flight tx index, the send journal, and
  **pending withdrawals**. Self-pruning.
- `data/` — wallets, their indexes, plus the token identity this database is
  denominated in. Catastrophic to lose.
- `log/` — deposits and **settled** withdrawals. Kept **forever**.

Records are hand-packed binary with a leading version byte: add a field by
appending and bumping the constant, read it under `if v >= N`, never reorder.
Nothing is deployed, so there is **no compatibility code anywhere** (§45) — enums
are densely numbered, no values are reserved, and nothing tolerates a record from
a shape that never existed. Do not add a legacy branch without naming the data it
is for. The version byte itself stays: it is the one thing that cannot be added
after there is data.
Sorted keys do real work — the deposit cursor *is* `<block><logindex>`, the send
journal is nonce-ordered, deposit dedup is a property of the key.

**Every composite key is fixed-width** (8-byte wallet id, 12-byte cursor), so
they concatenate with no separator. The `<slug>\0<rest>` scoping is gone with the
apps (§32) — do not reintroduce a variable-length key part without a separator.

**Secondary indexes are maintained by hand, inside the same transaction as the
record.** That is the cost of having no SQL, and it is where a bug would live.
`store/verify.go` exists to catch exactly that class.

## Wire contract

A wallet is addressed by the **ref its caller chose**, unique across the service.
There is no authentication and no tenancy: bsc is loopback-only and first-party,
so a key would buy nothing but ceremony, and a caller that needs a namespace
prefixes its own refs (§37).

Everything about one wallet hangs off `/v1/wallets/{ref}`; the two feeds
(`/v1/deposits`, `/v1/withdrawals`) are the only top-level collections, because
they are the only things not about one wallet. `PUT` creates — the caller owns
the identifier — and `PATCH` reconfigures.

**Every amount is the token's own base units**, as a decimal string. Never a JSON
number (a uint256 does not survive a float64), never a decimal point, never a
scaled figure.

**No machinery reaches the caller.** No nonces, gas, allowances, master address,
or internal flow state. `api/contract_test.go` enforces this by walking the real
surface and failing on the vocabulary — keep it passing rather than adding an
exception.

**Deposits have no status** (§50) — they are recorded once and never change.
Withdrawals have two, `pending` → `confirmed`, because a withdrawal is accepted
before it happens. Debits also carry a `reason` of `payout`, `fee` or `drain`. A withdrawal
has **no failure state** (§28): what cannot be honoured is refused synchronously
at creation and never becomes a record, and what breaks afterwards is ours to
retry — the commitment stays held and `attempts`/`last_error` are diagnostics.
This is why `ShouldPay` must keep its `RetryAfter` gate.

Callers **poll**; nothing is pushed. Both feeds are global and cursor-ordered —
`/v1/withdrawals` carries only *settled* debits, because it is a record of what
happened and a pending payout has not happened yet (§42). What a wallet still
owes is asked of the wallet.

The deposit feed has a cursor:
the chain's `(block, log_index)` internally, rendered as hex so it is
opaque-yet-ordered — compare two ids, never parse one. A deposit's `id` *is* its
cursor. Withdrawals are caller-initiated, so the caller polls its own outstanding
set by id.

## Fees

A fee is an optional field on a withdrawal request that writes a **second
debit**, to the master. bsc decides nothing about it — the caller passes a
number — and adds only joint acceptance, one idempotency key, and knowing the
destination (§43).

It is **not atomic**: two transfers are two transactions. Joint acceptance,
never joint settlement. Do not try to make it one record with two legs; that is
the `partial` status §24 retired.

## Gas is the operator's job

bsc spends native currency on every transfer. Fees land on the master (§43), so
a service that charges its users accumulates a gas budget there — but **nothing
converts it automatically**, and that is deliberate (§44).

Nothing in bsc converts them, and since §53 nothing can: the automatic top-up
went in §38 and the `bsc swap` command in §53. The master is the operator's own
wallet (§49), so trading from it belongs in whatever they already use — at a
price they looked at, with no extra surface inside a service that holds
everybody's keys. The loop is: alert on `bsc_master_bnb_wei`, run `bsc check`,
send native currency. Do not add trading back without re-reading §44 and §53.

`/healthz` reports a master below `gas.floor_wei` as degraded. Under §19 it
deliberately did not, because the automatic top-up made it self-healing; nothing
heals it now, so it is the finding.

## Docs (keep in sync when behavior changes)

- `docs/CONSUMING.md` — the integrator's guide: every endpoint, the cursor, the
  forward-or-accumulate decision. Update on any API change.
- `docs/ARCHITECTURE.md` — how it works plus a **numbered decision log**. Add an
  entry rather than silently changing a documented decision; §32–§40 are the v2.1
  retreat from the ledger and are the ones most likely to be re-argued.
- `docs/OPERATING.md` — day-2 runbook. `docs/ROADMAP.md` — optional backlog.
- `deploy/README.md` — systemd install and the full environment-variable table.

## Gotchas

- **Configuration is two files (§30).** `deploy/config.yaml` is every operational
  setting, documented, and safe to commit; `BSC_MASTER_SECRET` reaches the process
  through the environment and nothing else. Never put the secret in the YAML,
  never commit an example of it, and never write it anywhere but a secrets
  manager. It signs everything and holds a `MaxUint256` allowance on every managed
  wallet, so whoever has it can move every wallet's funds.
  Keys are grouped and the environment form is the key path upper-cased behind
  `BSC_` (`gas.floor_wei` → `BSC_GAS_FLOOR_WEI`).
  `TestShippedConfigMatchesTheDefaults` fails if the shipped YAML drifts from the
  binary's defaults — fix the YAML, don't weaken the test.
- `usdt/abi.go` is generated (`task abi`) — regenerate, don't hand-edit.
- **`ui_enabled` is off by default and that is a security boundary**, not a
  preference: the listener has no authentication, so the dashboard hands every
  wallet to anyone who can reach the port. Its `/ui/state` endpoint is the
  operator view and deliberately carries what the caller contract does not; it
  lives outside `/v1` so both stay true, and `api/contract_test.go` walks only
  the caller surface.
- **`money.drain_threshold_wei` is not a minimum deposit.** It decides only
  whether forwarding is worth the gas. Everything is recorded regardless (§36).
- **Anything signing from the master races the service for nonces** (§38). It
  is the same key and nonces come from the chain, so a hand-sent transfer during
  a busy window gets one of the two rejected. Harmless and loud; prefer a quiet
  moment.
- `chain.rpc_rate_limit` must clear the block rate by a wide margin. At ~0.45s
  blocks the watcher needs ~2.2 req/s just to keep up. Config refuses below 5.
- **There is no `CHAIN_ID`.** `chain.rpc_url` is the one chain input; `eth_chainId`
  after connecting decides the token, the router and the signer, and the answer is
  recorded in `data/` so a database cannot be opened against another chain (§26).
  `config.Parse` stays I/O-free; `Config.ResolveChain` is the step that needs the
  dial.
- ``chain.start_block: 0`` means "the current finalized head", not genesis.
  A watcher's *cursor* of 0, however, does mean block 0 — so `Step` refuses to
  run before `Start` has resolved it (§52). Do not remove that guard: the
  failure it prevents is silent, and looks like a service that is merely slow.
- The watcher must never be starved: it is what observes finality, so anything
  that blocks it stops every in-flight transfer too. Shutdown is decided by
  `ctx.Err()`, never by inspecting an error for `context.DeadlineExceeded`.
- Withdrawals are refused (503) while the watcher is more than
  `chain.max_lag_blocks` behind — accepting against a stale balance could
  overdraw a wallet.
