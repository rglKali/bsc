# Architecture & decision log

## Shape

One process, one store, four goroutine groups. They never call each other: work
is handed over as records, and the only direct coupling is a nudge from the
watcher to the sender so it need not poll.

```
                    ┌──────────────────── bsc ─────────────────────┐
                    │                                              │
  BSC RPC ◀────────▶│   chain — one dial · one rate limiter        │
                    │      ▲                      ▲                │
                    │      │ finalized blocks     │ sign & send    │
                    │   watcher                sender              │
                    │      │                      │                │
                    │      ▼                      ▼                │
                    │  ┌──────────────────────────────────────┐    │
  caller (HTTP) ───▶│  │  bbolt: state/ · data/ · log/        │    │
                    │  └──────────────────────────────────────┘    │
                    │      ▲                      ▲                │
                    │     api                 snapshotter          │
                    └──────────────────────────────────────────────┘
```

| Role | Does | Driven by |
| --- | --- | --- |
| watcher | finalized head → block receipts → match → **one write transaction** per block | poll interval |
| sender | claims the work a flow's state owes: signs, journals, broadcasts | a nudge, or a tick |
| api | the HTTP surface plus `/metrics`, `/healthz` and — when enabled — `/ui/` | requests |
| snapshotter | periodic consistent backups | interval |

## What this service is, and is not

bsc is a **low-level primitive**. It derives wallets, observes what lands on
them, forwards the ones configured to forward, and executes payouts on request.

It is deliberately *not* a payments provider. It keeps no ledger, charges no fee,
and has no notion of an app, a user or an account. It knows what the chain says
about addresses it derived, and nothing else. A service that needs to know whose
money is where keeps those books above bsc, which is the shape that lets this one
stay small and auditable (§33).

That is a reversal. v2.0 grew an internal cents ledger, a fee policy, a house
sweep and an automatic gas top-up, and each was individually reasonable — but
together they made bsc a full service with a chain underneath rather than a
chain primitive. §32 through §40 record the retreat.

## The store is the design

Everything is one bbolt file in three namespaces:

```
state/   cursor · flows · in-flight tx index · send journal        self-pruning
data/    wallets · their indexes · the token identity              catastrophic to lose
log/     deposits · withdrawals                                    kept forever
```

Records are hand-packed binary — a 20-byte address is 20 bytes, an amount is its
native 32-byte big-endian form — each with a leading version byte, so a new field
will be an append rather than a migration. Nothing has been deployed, so nothing
yet reads a version other than 1; the byte is there because it is the one thing
that cannot be added cheaply later, since giving existing records a version is
itself the migration it exists to avoid. Every enum is densely numbered and no
value is reserved: there is no data from a previous shape to avoid colliding
with (§45). Sorted keys do real work: the deposit
cursor *is* `<block><logindex>`, the send journal is nonce-ordered per signer,
and deposit dedup is a property of the key rather than a constraint to check.

Every composite key is built from **fixed-width** parts (a 16-byte wallet id, a
12-byte cursor), so they concatenate unambiguously with no separator. The
`<slug>\0<rest>` scoping the app model needed is gone along with the apps (§32).

Having one store is what makes a finalized block atomic. Applying one commits, in
a single transaction: confirmations for transactions that landed, balance changes,
deposit records, any new work the rules imply, and the cursor. There is no second
system to keep in step, so there is nothing to reconcile afterwards and no dedup
window to tune.

The cost is hand-rolled secondary indexes, maintained inside the same transaction
as the record they index. `bsc inspect` exists to catch exactly that class of bug.

### How big it gets

Measured, with 100k managed wallets in a real store:

| | |
| --- | --- |
| the watched-address set | **5.3 MB** of heap (`map[Address]uuid`, ~40 B/entry) |
| resident after loading it | ~40 MB, the rest being bbolt's mmap made resident by the scan — file-backed and evictable, not memory we hold |
| the database file | 68 MB — a 150-byte wallet record, plus its two lookup indexes and bbolt's ~50% leaf fill on random-order inserts |
| startup | `AddrSet.Load` 14 ms, `EvaluateAll` 27 ms |
| per block | **unchanged by wallet count** |

That last row is the one that matters. `eth_getBlockReceipts` returns a whole
block and every log is tested against the in-memory set, so the RPC side never
learns how many addresses we watch — there is no filter to hand 100k addresses
to. The rules then visit only the wallets a block touched
(`watcher/apply.go`, `evaluateTouched`) and the wallets with an open withdrawal,
so per-block cost is proportional to activity.

The map grows in powers of two, so its cost is a step function rather than a
line: 100k sits in a 131,072-slot table, ~115k tips it to the next and roughly
10 MB, where it stays until ~230k. A million addresses measures 84 MB.

What actually grows without bound is `log/`, which is kept forever: a deposit is
138 bytes plus about as much index again.

## Pipelines: sequential execution, persisted waiting

Every piece of chain work is a **flow**: a persisted state machine.

| Flow | States (skipping to the last if the wallet is already active) |
| --- | --- |
| `transfer` | `funding` → `approving` → `moving` → `done` \| `failed` |
| `prewarm` | `funding` → `approving` → `done` |

A transfer is a drain or a payout depending on one thing: whether it carries an
amount and the withdrawal id that goes with it. Set, and it pays that withdrawal
exactly; unset, and it sweeps whatever the wallet holds. They were two kinds
running byte-identical state machines until §46.

These are **internal** and no caller ever sees one. The overlap with the
caller-facing vocabulary is accidental and worth keeping straight: a flow's
`failed` is one attempt giving up, which for a withdrawal means a backoff and a
retry, not a verdict — a *withdrawal* has no failed status at all (§28).

Activation is not a separate flow — funding and approving are simply the prefix
of whatever needed an inactive wallet.

A flow's **state is the instruction**. `funding` means "the funding transfer still
needs to go out", and the record carries everything that transfer needs, so
committing a state change *is* enqueueing the work and a crash in between
re-derives the same action. Nothing waits in a goroutine; waiting is a row.

Execution is deliberately sequential — one transaction in flight at a time. The
event model buys crash recovery, not concurrency.

## Work is declared, not enqueued

Rather than "a deposit creates a drain job", the service states what should exist
and converges on it. The rules are evaluated inside the block transaction, right
after any flow terminates, and once at startup:

> a wallet with a **`drain_to`**, a balance at or above `money.drain_threshold_wei`,
> no live flow, and past its retry deadline is owed a **drain**
>
> a wallet **without** a `drain_to`, with a pending withdrawal, no live flow, not
> paused, and past its retry deadline is owed a **withdrawal**

Those two are mutually exclusive by construction, which is the point of putting
the topology on the wallet: there is no ordering to get right between them and no
layer above them to arbitrate (§32).

Two deposits in one block are applied before the check runs, so the wallet is
evaluated once with the summed balance and exactly one drain starts. A deposit
landing *during* a drain finds the wallet busy and starts nothing — and the
evaluation after that drain terminates sees the leftover and drains it, so funds
cannot be stranded by arriving between the balance read and the signature.

Since the rules read current state rather than react to events, the startup pass
converges whatever was missed while the process was down.

## Money

There is one quantity and one unit: **custody**, in the token's own base units.

```
custody   what the address holds, accumulated from every Transfer observed
committed what pending withdrawals have promised out of it
available custody − committed
```

The chain can say how much a wallet holds. It cannot say whose it is — and bsc no
longer pretends to know, because nobody ever told it. `available` is not a
statement about ownership; it is the most a new payout may ask for without
overdrawing the address.

That overdraft guard is the whole of what replaced the reservation ledger:
`committed + amount ≤ balance`, computed from records that already exist, checked
before anything is signed (§39). There is nothing materialised to keep in step,
so there is nothing that can drift.

Nothing is scaled and nothing is rounded. Flooring to cents was the only rounding
in the service and the only place value could go missing; with the chain's own
units there is nothing to floor, no dust, and no house to round towards (§36).

## Nothing escapes us, with one exception

Every payable address is derived by us, and every USDT `Transfer` is a log with
`from`, `to` and `value`, so deposits, drains and payouts are all observed. USDT
on BSC has no rebase, no fee-on-transfer and no balance-mutating admin hook, so
the sum of observed transfers *is* the balance.

The exception is **native BNB arriving at the master** — an operator's manual gas
top-up is a plain value transfer, which emits no log at all. That is a single
`eth_getBalance` gauge, not reconciliation, and it is the number that matters
most operationally: a dry master stops every pipeline, and since §38 nothing
refills it without being asked.

There is therefore no reconciler process. Correctness is checked where it counts:
`balanceOf` immediately before spending, and `bsc inspect` on demand.

## Decision log

Add an entry rather than silently changing a documented decision.

1. **One process, one store.** The v1 daemon/wallet/gateway split is merged;
   Postgres and NATS both left. The payoff is a single RPC budget, no duplicated
   state at the seams, and a block becoming one atomic transaction — which
   retires the two-phase commit and dedup window that existed only because
   publishing and the cursor lived in different stores.
2. **bbolt, not Postgres, not badger, not SQLite.** Megabytes and a few writes
   per second want a memory-mapped B+tree with no compaction and no tuning.
   Records are hand-packed binary, which is also the answer to ORM type friction:
   no generator, no overrides, no re-serialization.
3. **Three namespaces in one file, not three stores.** The separation is real,
   but separate stores would put the dual write straight back into the block
   commit.
4. **No encryption at rest.** The database is not the crown jewel: the master
   secret never enters it, anyone who can read the file is already on the box
   where that secret lives, and the secret alone is total loss regardless.
   Encrypt snapshots that leave the box instead.
5. **Sequential execution; the event model is for recovery, not concurrency.**
   Waiting is a persisted state rather than a blocked goroutine, so a restart
   mid-wait reads records instead of reconstructing intent. The ceiling is ~1
   transaction per finality window, which at 0.45s blocks is a second or two.
6. **One shared RPC limiter, no reserved lane.** The signing path is a handful of
   calls behind a single in-flight transaction, so it cannot starve anything, and
   a FIFO budget makes priority inversion against the watcher structurally
   impossible. The watcher is the heavy user (~2.2 req/s), so the limit must
   clear the block rate by a wide margin or catch-up never converges.
7. **Nonces are read from the chain, never counted locally.** The operator signs
   by hand for gas top-ups, and a stored counter would silently desync.
8. **One live flow per wallet, held as a pointer.** Makes "is this wallet busy?"
   a field read, makes the invariant structural, and lets activation be the
   prefix of whatever flow needed it rather than a separate parked flow.
9. **Work is declared, not enqueued.** Converges without a dirty bit, and cannot
   strand a deposit that lands mid-flight.
10. **`balanceOf` is the authority at sweep time; the deposit is only a trigger.**
    Superseded in part by §22: the chain is still the authority for what can
    physically move, but no longer for what an app may spend. The threshold this
    once described was split in two — see §23.
11. **Gas is estimated, never configured.** A deposit wallet is funded once for
    one `approve` and never needs BNB again. Only the two multipliers remain,
    because estimates go stale.
12. *(A ref identifies a wallet now — see §37 — but the no-keys reasoning is unchanged.)* **A slug identifies an app; no API keys.** Loopback-only, first-party
    callers. Registration is public and idempotent; apps configure themselves.
13. **No admin API and no UI.** With slug addressing the app API already is the
    operator's read surface. Write-side operator actions did not survive
    scrutiny: master BNB is a gauge, retrying a genuinely failed transfer cannot
    succeed, and pausing a first-party app is something the app can do itself.
    *(The "no UI" half is superseded by §29: there is an optional dashboard now,
    off by default. The "no admin API" half stands — it adds no operator write
    the app API did not already have.)*
14. **No reconciler.** Everything on the USDT side is log-derivable. The one
    exception — native BNB arriving at the master — is a gauge. Correctness is
    checked at the point of spending and by `bsc inspect` on demand.
15. **The withdrawal fee is a business fee**, app-configured with no operator
    floor — one transfer per withdrawal instead of two, which retires `partial`,
    the only status that ever needed manual repair. `min_cents` doubles as the
    brake on withdrawal spam. How the fee is *collected* changed twice since:
    accrued and swept in batch, then taken sequentially after each payout, and
    now not moved at all — see §24.
16. **Polling two read models, not webhooks or a broker.** Deposits are
    unsolicited so they get a cursor — the chain's own `(block, log_index)`,
    which every deposit has by construction. Withdrawals are app-initiated, so
    the app polls its own outstanding set by id. *(The shape of both held; only
    the spelling changed. §27 made the cursor opaque hex rather than
    `<block>-<logindex>`, and the set an app polls is now `?status=pending`.)* Webhooks would mean owning per-delivery
    state, attempt counters, backoff timers and dead-lettering — and would still
    need a cursor behind them for recovery. A broker would put back the moving
    part this design removes. SSE remains a small addition on the same substrate
    if a poll interval ever proves too slow.
17. **Deposit addresses are permanent per ref.** No reuse, no rotation, no expiry.
18. **Retention.** `state/` self-prunes as flows terminate; deposits and
    withdrawals are kept forever; there is no third log to age out.
19. *(Withdrawn by §38: trading is an operator command now.)* **Gas top-ups swap collected fees back into native currency**, reversing v1's
    decision that gas replenishment stays manual. That decision reasoned the
    operator holds the key and can swap by hand — true, but it weighed an
    attended service. The top-up is a flow like any other, so it inherits the
    journal, the sequential nonce lane and the audit; it is off unless a router
    is configured; and it is bounded by a cooldown, because a swap that succeeds
    without lifting the balance above the floor would otherwise trade away every
    fee. It is the only operation whose outcome is a price rather than a yes or
    no, so it is also the only one with a slippage bound and a deadline.
20. *(Still true, but of `bsc swap` rather than of a flow — see §38.)* **The swap uses the Uniswap-V2 router interface**, not a Universal Router,
    despite newer venues existing on this chain (PancakeSwap V3 SmartRouter and
    Infinity, Uniswap v4). The trade is ~10 USDT at most once an hour, where V2's
    0.25% fee costs a couple of cents more than V3's best tier — well inside the
    1% slippage bound we would accept anyway. Against that, a Universal Router
    means Permit2 approvals and command-byte encoding: a second protocol and
    several hundred lines, in the one code path that can succeed and still give a
    worse result than expected. If routing quality ever matters, the next step is
    the V3 SmartRouter, which keeps a plain ERC-20 approve; the Universal Router
    is the step after that.
21. **Withdrawals are refused while the chain sync is behind.** Balances are only
    current at the head, and reserving against a stale one could overdraw an app.
    Reads keep working; only spending is held back.
22. *(Reversed by §33. Kept in full because the reasoning is sound for a payments provider and is exactly what bsc decided not to be.)* **An app's balance is an internal ledger, not a chain balance.** The chain can
    say how much a wallet holds; it can never say whose it is. One hot wallet
    carries the app's money, the fees we have charged and sub-cent dust in a
    single number, and every attempt to make that number mean "what the app can
    spend" has to subtract things it cannot see. So ownership is tracked in a
    ledger — credited deposits less settled withdrawals, in cents — and the
    wallet balance means custody and only custody.

    This reverses the earlier decision that `balanceOf` is the authority for what
    an app may spend, and it is worth being explicit about what that costs: our
    own record is now the authority, so a ledger bug is directly a money bug
    where before the chain was a second opinion. Three things pay for it. The
    ledger is a pure function of `log/`, so `bsc inspect` recomputes it and
    compares — the check the old materialised balance could not support at all.
    Solvency is checkable for the first time: custody ≥ ledger, per app, with the
    difference being the house's. And `balanceOf` remains the authority at
    *signing* time, so authorisation moved to the ledger while execution did not.

    A deposit joins the ledger when its **drain lands**, not when it is seen.
    Money still sitting in a deposit address cannot be paid out of the hot
    wallet, so crediting it earlier would authorise a payout with nothing behind
    it. Until then it is `pending`.
23. *(Reversed by §36.)* **Cents on the wire; wei only where we touch the chain.** Every amount an app
    sends or receives is a whole number of cents as a decimal string. Flooring to
    cents happens exactly once, when a transfer is observed, and always rounds
    towards the house. The ledger is therefore exact — every entry is a whole
    cent by construction — while custody, gas and swaps stay in wei and are
    compared to the chain bit for bit.

    The scale comes from the token's own `decimals()`, read at startup: a cent is
    10^16 wei for BSC's 18-decimal USDT and 10^4 for a 6-decimal one, and a
    constant that can be wrong eventually is. The token and its decimals are
    recorded in the database on first run and refused if they later change, since
    every amount stored is denominated by them.

    Transfers worth less than a cent get **no record at all**. `MIN_DEPOSIT_WEI`
    is gone: a threshold that decided whether a user was credited was a threshold
    that could silently confiscate most of a dollar. What replaces it,
    `money.drain_threshold_wei`, decides only whether moving the money is worth the
    gas — the app is credited either way, and sees the difference as `pending`.
24. *(Withdrawn by §33: there is no fee.)* **A withdrawal is one transfer again, and the fee is collected by not paying
    it.** The fee leaves the app's ledger with the payout but never moves on its
    own: it stays in the hot wallet, now belonging to the house. This supersedes
    the sequential payout→fee flow, which cost a second transaction and a second
    settlement path per withdrawal, and it deletes the accrual namespace, the
    fee reservation and the `collecting` state along with it. Payout and fee now
    share one reservation because they share one lifetime.
25. *(Withdrawn by §33: bsc cannot compute an excess it has no ledger for.)* **One rule collects everything nobody is owed.** After the ledger, the fees we
    charged, the sub-cent remainders flooring left behind, and any tokens a
    stranger sent to a managed address are the same quantity: the excess of
    custody over the ledger. A single `house_sweep` flow moves it to the
    collector once it is worth a transfer, and defers to any pending payout.

    "Everything" is the top-level wallet's excess, which is narrower than it
    sounds: the sweep is scoped to `KindTopLevel` and the drain to `KindDeposit`
    above `money.drain_threshold_wei`, so a deposit address that only ever receives
    sub-threshold amounts holds them indefinitely — its dust never reaches a
    wallet the sweep looks at. Later deposits push it over and carry the old dust
    along, so this only strands anything on an address that is never used again.
    It is the drain threshold working as intended (moving three cents costs more
    than three cents), not a leak, but the quantity is not literally *all* of the
    house's money.

    Its amount is resolved at signing time from a `balanceOf` and the ledger read
    together, never fixed when the flow starts — it is the one operation whose
    amount is a computation over somebody else's money, so a stale reading must
    leave money behind rather than take money that was owed. A negative excess is
    an alarm, not a number to act on: the sweep refuses and raises
    `bsc_solvency_shortfalls_total`.

    **Open question — what the dust is ultimately for.** Today it accrues and is
    swept as house income, which is standard and is documented in the integrator
    contract. Whether some of it should instead be returned, or held against
    rounding in the other direction, is deliberately unsettled; nothing in the
    design depends on the answer, because the excess is derived rather than
    stored and can be redirected without a migration.

26. **The endpoint says which chain this is; `CHAIN_ID` is gone.** It used to be
    the setting everything else followed — the endpoint, the token and the swap
    router all defaulted from it. Nothing checked it against the endpoint, and
    the default was mainnet, so pointing `chain.rpc_url` at testnet and forgetting it
    produced a service that synced blocks, recorded deposits correctly, and had
    every single transaction rejected: the signer binds each one to the
    configured id (EIP-155), so the node refuses them all. Reads work, writes
    fail, and nothing in the logs says why.

    Now `chain.rpc_url` is the one chain input. `eth_chainId` after connecting decides
    the token, the router and the signer, so there is no second value left to
    disagree. `config.Parse` still does no I/O and still refuses a malformed
    address before anything dials; `Config.ResolveChain` fills in what only the
    chain can answer. An explicit `chain.token_address` or `swap.router` still wins,
    and an endpoint we have no defaults for is a startup error naming exactly
    what is missing.

    The chain id joins the token and its decimals in `data/`, checked on every
    later start. A database built against one chain cannot be opened against
    another — the wallets in it were derived for that chain and the amounts are
    denominated in its token, so moving it is a migration, not a config edit.

27. *(Recast by §36 and §37: the surface is a wallet's, and the chain's own unit is now on the wire — but the ban on machinery, and `api/contract_test.go` enforcing it, stand.)* **The app-facing API is a payments provider's, not a chain's.** An app asks
    for an address, is told what arrived and what it may spend, and asks for a
    payout. Everything about *how* that happens on-chain is now absent from the
    wire: the wei amount beside every cents amount, the block height and log
    index on every deposit, the hash of the drain that swept it, and the
    `/wallets` noun for something an app only ever treats as an address.

    The reasoning that put `amount_wei` there (§23) was that an operator should
    be able to reconcile a record against a block explorer. That is a real need
    and it was solved in the wrong place: `tx_hash` already opens the exact
    transfer on bscscan, and `bsc inspect --rpc` reconciles the whole database
    against the token. Neither needs the app to carry a second unit it must
    never do arithmetic on — and a wei figure on the wire is an invitation to
    try. Cents, a hash, a status: the app has no use for more.

    The deposit cursor stays the chain's `(block, log_index)` internally,
    because it is still the only ordering every deposit has by construction, but
    it is rendered as hex rather than `<block>-<logindex>`. Hex keeps the one
    property an app may rely on — two ids compare in the order the chain
    produced them — while removing the one it must not, which is reading a block
    height out of a cursor and reasoning about the chain's position. It is
    reversible on purpose: opacity is a contract with the app, not a secret, and
    support still needs to find a deposit.

    `api/contract_test.go` walks the real surface and fails on the vocabulary
    itself, because "no wei on the wire" is a property of every response rather
    than of any one handler, and is exactly what a well-meaning addition puts
    back without noticing.

28. **A withdrawal has no failure state.** The lifecycle is `pending` →
    `debited`, mirroring a deposit's `pending` → `credited`: two words each,
    both terminal ones naming what happened to the app's balance rather than
    what happened on the chain. `queued`, `pending`, `done` and `failed` are
    gone, along with the internal `funding` / `approving` / `draining` states
    that were never app-visible anyway.

    The split is by *whose problem it is*. A request that cannot be honoured is
    the app's problem and is refused synchronously, so it never becomes a record
    at all — a bad address, an amount under the minimum, a fee that exceeds it,
    a paused app, an overdraft, a watcher too far behind. Anything that goes
    wrong after we have accepted it is ours: a reverted payout keeps its
    reservation, stays `pending`, stamps `attempts` and `last_error` as
    diagnostics, and backs the wallet off for the rules to retry. The app is
    never handed a terminal state it has to compensate for, because the only
    terminal state is the money having moved.

    This makes `ShouldPay`'s `RetryAfter` gate load-bearing rather than
    symmetry with `ShouldDrain` and `ShouldSweepHouse`. It was safe to omit only
    while a reverted payout terminated; with the retry it is the one thing
    standing between a payout that always reverts and re-signing it on every
    evaluation, burning the master's gas as fast as blocks arrive.

    **What makes retrying-forever safe is that almost nothing is permanently
    unpayable.** A plain BEP-20 `transferFrom` does not call the recipient —
    there is no hook, unlike ERC-777 or ERC-1363 — so the destination gets no
    opportunity to reject it; the token updates two balance slots and that is
    all. A contract, a cold wallet and an EOA are the same thing to it. Every
    ordinary payout failure is therefore *ours* and transient: a hot wallet
    momentarily short, an allowance not yet in place, a node that refused the
    broadcast. Those are precisely what should retry rather than be handed back.

    The one exception a valid address can be is the **zero address**, which
    `common.IsHexAddress` accepts and the token reverts on. It is refused at
    creation, where it costs no RPC call — the rule being that anything
    permanently unpayable must be caught *before* it becomes a record holding a
    reservation, because there is no terminal state left to settle it into
    afterwards.

    That leaves one residual, and it is a property of the token rather than of
    this design: a token with a transfer blacklist can make one specific
    destination permanently unpayable. The default BSC deployment (Binance-Peg
    BSC-USD) has no such list; mainnet Ethereum Tether does. Since
    `chain.token_address` is configurable, the posture is to retry and make it visible
    rather than to guess — `attempts` climbing into double digits is an
    operator's signal, not a retry's, and the remedy today is editing the
    record. A cancel is the obvious follow-up and is deliberately unbuilt
    (`docs/ROADMAP.md`): releasing a reservation for a payout that might still
    land is the one way this design could pay twice.


29. **An optional dashboard, off by default.** `ui_enabled: true` mounts a plain
    HTML/CSS/JS page at `/ui/`, for driving the service by hand in a sandbox and
    for watching it work in real time. It is embedded in the binary, so there is
    still one artefact to ship and no build step to keep alive.

    §13 said no UI, and the reasoning there was about *write* actions: nothing an
    operator needed to do was missing from the app API. That still holds, and the
    dashboard adds no write of its own — every button on it calls the same
    `/v1/wallets/...` endpoints an integrating service calls, so what you watch
    work there is exactly what a caller gets. What §13 undervalued is reading: `curl`
    against six endpoints, `/metrics` in Prometheus text, and a flow table that
    was not exposed at all is a poor way to answer "why has nothing moved for ten
    minutes".

    So the page has one endpoint of its own, `GET /ui/state`, and it is
    deliberately everything the app contract refuses to carry (§27): custody in
    wei beside the ledger in cents, the excess and whether it has gone negative,
    the block height and lag, and the live flow list with each one's state,
    attempt and backoff. That is the operator's view, and keeping it at `/ui/`
    rather than under `/v1` is what lets both contracts be true at once —
    `api/contract_test.go` walks only the app surface, and
    `TestDashboardDoesNotChangeTheAppContract` asserts that turning the dashboard
    on changes not one byte of it.

    **Off by default is the security boundary, not a preference.** This listener
    has no authentication and never will (§12), so the page is as trusted as the
    network it is reachable from — and unlike an app's slug, which buys the
    holder one app, the dashboard hands over every app at once. Enabling it logs
    a warning saying so. It belongs on a laptop and on a box only the operator
    can reach; it does not belong anywhere else.


30. **A reviewable config file, and one secret in the environment.** Settings
    were environment-only, which meant `BSC_MASTER_SECRET` and twenty-five
    operational values shared one file. That file therefore had to be `640
    root:bsc` and could never be committed — so the thresholds, fee policy and
    intervals this service moves money on were unreviewable, and "what changed
    last month" was unanswerable.

    Now `deploy/config.yaml` carries everything operational, documented per
    setting, and `BSC_MASTER_SECRET` is the one value that reaches the process
    through the environment. The split is a security boundary in one direction
    only: the secret must not be in the file, but the file gains nothing from
    being secret. Keys are grouped — `chain`, `money`, `gas`, `swap`,
    `snapshot` — and each is addressable either way, the environment form being
    the key path upper-cased behind `BSC_`. Precedence is flag, environment,
    file, default, so one value can be overridden during an incident without
    editing a file someone reviewed.

    The repo ships `deploy/config.yaml` and no environment file at all. On a box
    the secret lives in `/etc/bsc/bsc.env` (0640 root:bsc), which the unit loads
    and which holds that one value — but there is nothing to copy it from here,
    because the only thing such a template could contain is the secret itself.

    Two things were deliberately not done. The `Config` struct stayed flat and
    `Parse` kept its explicit `v.GetX` calls rather than becoming an
    `Unmarshal` with `mapstructure` tags: the per-setting validation is the
    valuable part — "swap.slippage_bps 10000 would accept any price at all"
    beats a decoder error — and reflection would have cost that while forcing
    every consumer to learn a nested shape. And `Parse` still does no I/O; the
    file is read while viper is being built, so the property §26 relies on —
    that configuration fails on a bad value before anything dials — is unchanged.

    `TestShippedConfigMatchesTheDefaults` parses `deploy/config.yaml` on every
    test run and fails if any value in it stops matching the binary's default,
    because a documented surface that has drifted is worse than none: it is the
    file an operator copies and trusts. It caught a wrong default the first time
    it ran.


31. **A health check that can fail, and a build you can name.** `/healthz` used
    to answer a static `ok`, which told you the process was running — the one
    thing a refused connection had already told you. It now reports the three
    conditions that leave the service answering requests while unable to do its
    job: a store that will not answer, a sync far enough behind that withdrawals
    are being refused (§21), and a master that cannot pay for gas.

    That last check is a conjunction on purpose. A master below the gas floor is
    the *normal* case — the top-up sells collected fees back into gas without
    being asked, and reporting it would cry wolf on something that fixes itself
    routinely. It is a finding only when it is low **and** cannot recover: either
    swapping is off, or it holds less than `swap.amount_wei` to sell. That is the
    same combination `deploy/README.md` builds the Grafana master panel around,
    and it is the one an operator must not sleep through.

    The native balance is handed in rather than read, because it is the one
    quantity in the service that cannot be derived from logs: a manual gas
    top-up is a plain value transfer and emits nothing (§14). The watcher
    publishes its last observation, and "not yet observed" stays distinct from
    zero so a fresh start is silent rather than degraded.

    `/healthz` is for monitoring, never for restarting. Nothing it reports is
    fixed by starting the process again, and a restart mid-sync makes the lag
    worse.

    Alongside it, `bsc/buildinfo` holds the version, stamped at link time. It
    previously travelled main → `cli.SetVersion` → `app.Version` →
    `api.Options`, and each hop was somewhere one of them could end up reporting
    a different build from the one running.

    Not taken from the same source: per-route HTTP metrics and a request
    middleware. Both earn their place on a public edge; this surface is
    loopback-only and first-party, and `/metrics` already carries every quantity
    that decides whether the money moves.



---

## The v2.1 retreat: from provider back to primitive

Everything from here supersedes something above. The short version: v2.0 added a
ledger so that bsc could answer "what may this app spend", and answering that
question turned out to require knowing who the app's users were, what it charged
them, and what share of a wallet was the house's. None of that is a chain
problem. Removing it is §32 through §40.

32. **The topology is a field, not a type.** `KindTopLevel` and `KindDeposit` are
    replaced by one nullable `drain_to` on the wallet. A wallet with one forwards
    everything it receives; a wallet without one keeps it and can pay out.

    This supersedes §4's note that the drain rule is scoped by kind. It is now
    scoped by whether there is anywhere to drain *to*, which is the same guard
    expressed as data — and a strictly stronger one, since a wallet with no
    destination cannot be told to move its balance to itself under any rule.

    What it buys is every topology the two-level shape could not express: chains
    (`leaf → middle → vault`), fan-in from many wallets to one, a wallet that is
    both a collection point and a payout source, and retargeting any of it
    without a migration. What it costs is §35.

    Forwarding and accumulating are **mutually exclusive**, enforced rather than
    documented: a withdrawal from a forwarding wallet is refused, and a wallet
    with a pending withdrawal cannot start forwarding. They contradict each other
    — one empties the wallet, the other spends from it — and both racing for the
    same funds is how a payout reverts.

33. **The ledger, the fee and the house sweep are all gone.** §22 made an
    internal cents ledger the authority on what an app may spend. §24 collected a
    fee by not crediting it. §25 swept the excess of custody over that ledger to
    a collector. All three are removed, and so is the `App` record they hung off.

    The reasoning that put them there still holds *for a payments provider*. The
    error was that bsc is not one. Each addition was small and each was correct
    in isolation; together they meant this service had to know who an app's users
    were, what it charged them, and which share of a wallet belonged to whom —
    questions a chain gateway has no way to answer and no business asking.

    The replacement is a service boundary: whatever keeps the books does so above
    bsc, and asks bsc to move money. A fee becomes two withdrawals — one to the
    user, one to yourself. The house's share becomes a withdrawal to wherever you
    keep it. Both are things the caller already knows how to express, and bsc no
    longer needs a concept for either.

    The cost is honest: bsc can no longer tell you it is solvent for a given
    tenant, because it does not know what a tenant is. What it can still tell
    you, and now checks, is that no wallet has promised more than it holds (§40).

34. **A deposit's status says where the money is, not who is owed it.**
    `pending → credited` becomes `received → forwarded`. The words changed
    because the meaning did: `credited` was a statement about a ledger, and
    there is no ledger to be credited to.

    A deposit on a wallet that accumulates stays `received` forever. That is
    terminal, not unfinished — the money is exactly where it was meant to land.

    A consequence worth stating: when a wallet forwards into another managed
    wallet, the arrival at the far end is recorded as a deposit too. Under the
    ledger this had to be suppressed, because crediting both ends would have
    double-counted; with no ledger there is nothing to double-count, and the two
    records are genuinely two observations of two transfers. The upstream
    record's `drain_tx` equals the downstream record's `tx_hash`, so a caller
    counting money can pair them.

35. **Arbitrary topology means cycles, so cycles are refused.** `A → B → A` moves
    the same money round and round. Every individual transfer succeeds, so
    nothing surfaces as an error; the only symptom is the master's gas draining
    at the rate the chain produces blocks.

    The two-level design made this unrepresentable, which is a real property §32
    gave up. It is bought back with a check at write time: setting a `drain_to`
    walks the chain of references and refuses one that returns to the wallet
    being configured, or runs deeper than `MaxDrainDepth`. A destination bsc does
    not manage ends the walk — somebody else's address cannot point back at us.

    `Verify` re-checks it offline, because a record edited outside the service
    would bypass the write-time guard, and this is the failure that costs money
    silently.

36. **One unit: the token's own.** §23 put cents on the wire and made
    `money.Scale` the single bridge to wei. Both are gone. Amounts are the
    integer the token itself moves, as a decimal string, in every direction.

    Cents existed to make the ledger exact. With no ledger, the conversion is
    pure loss: flooring was the only rounding in the service and the only place
    value could go missing, and it existed solely so that a sub-cent remainder
    could be called the house's. There is no house now.

    So there is no dust, no minimum, and no `MIN_DEPOSIT_WEI` successor. A
    one-unit transfer is recorded exactly like a thousand-token one.
    `money.drain_threshold_wei` survives and means only what it always claimed
    to: do not spend gas moving less than this. It never decides what is
    recorded.

    The token's `decimals()` is still read and recorded in `data/`. Nothing
    scales by it any more; it is what lets `bsc check` and the dashboard render
    a raw integer as something a human can read, and it keeps the database's
    identity complete.

37. **No tenancy, and the caller owns the namespace.** With apps gone there is
    nothing to scope by, so `ref` is unique across the whole service, the deposit
    feed is global, and so is the idempotency namespace.

    This is a real loss of a real property: two callers sharing one bsc can now
    collide on a ref or a key, and either can read the other's wallets. The
    honest framing is that §12's threat model always said as much — loopback
    only, first-party callers, no authentication — and the per-app scoping was
    never a security boundary, only a convenience. A caller that needs one
    namespaces its own refs (`acme:cust-1`), which is a line in its code rather
    than a concept in this service.

38. **Trading is an operator command, not a flow.** §19 made the gas top-up
    automatic, reasoning that an unattended service must be able to refill
    itself. §20 chose the router for it. Both are withdrawn.

    The immediate reason is §33: the top-up sold *collected fees*, and there are
    no fees. But the better reason is the one §19 half-admitted — it is the only
    operation whose outcome is a price rather than a yes or no, and it fired at
    whatever moment a balance crossed a line, at whatever the market happened to
    be. An attended trade at a price somebody has just looked at is strictly
    better, and the automation was buying very little: a top-up is a rare event
    with hours of warning.

    What replaces it is `bsc check` (balances, the floor, the gas price, and a
    live quote in both directions) and `bsc swap` (`--sell-usdt` for an exact
    input, `--buy-bnb` for an exact output), plus the `bsc_master_bnb_wei` gauge
    to alert on. `/healthz` reports a master below the floor as degraded, which
    §31 deliberately did *not* do — because under §19 it was self-healing and
    reporting it would have cried wolf. Nothing heals it now, so it is exactly
    the finding.

    The trade-off taken knowingly: `bsc swap` signs with the same key the running
    service signs with, and nonces come from the chain rather than a counter
    (§7). Run it while a transaction is in flight and one of the two is rejected.
    That is recoverable and loud, which is the right side to fail on for a
    command a human runs a few times a year.

39. **The overdraft guard is a comparison, not a balance.** `ReserveLedger` /
    `ReleaseLedger` are replaced by: the sum of a wallet's pending withdrawals,
    plus the amount being asked for, against what the chain says it holds.

    Both sides are records that already exist, so there is no reservation to
    materialise, nothing to release on settlement, and no way for a stored figure
    to drift from what it summarises. A reverted payout needs no special handling
    either — it stays pending, so it stays committed, which is exactly right.

40. **The audit checks structure and solvency per wallet.** With no ledger to
    recompute from `log/`, `Verify` changed shape: it walks every index in both
    directions, checks that no wallet claims a flow that does not exist and no
    flow owns a wallet that claims another, checks that no drain chain loops, and
    checks that no wallet has promised more than it holds.

    The last is what became of §22's solvency margin. It is narrower — it cannot
    tell you a tenant is covered, because tenants are not a thing here — but it
    is also stricter in the one way that matters, being asked of the address that
    actually holds the money rather than of an abstraction above it.


41. **Activation is lazy again, and creating a wallet spends nothing.** Deriving
    a wallet is HMAC over the master secret and a write to bbolt. It signs no
    transaction and pays no gas. The funding transfer and the approve happen as
    the prefix of whichever flow first needs to move money — a drain when the
    first deposit lands, a withdrawal when the first payout is asked for.

    v2.1 briefly got this wrong. The old design pre-warmed an app's *top-level*
    wallet at registration and left deposit addresses lazy, which was right for
    both: there was one top-level per app and it was certain to be used, while
    most of an address book might never receive anything. Collapsing the two
    endpoints into `POST /v1/wallets` (§32) carried the prewarm onto every
    wallet, so a caller registering ten thousand users who never deposit would
    have paid for twenty thousand transactions against money that never arrived.

    The cost of lazy is latency, and it falls where it is cheapest: the first
    movement is three transactions instead of one. For a deposit that is
    invisible — the money is already recorded and the caller is not waiting on
    the forward. For a payout it is visible, which is what `prewarm: true` on
    create is for: a treasury you are about to draw on, where you know the
    activation is coming anyway and would rather not pay for it in the middle of
    the first withdrawal.

    The flag is off by default and deliberately narrow. The rule is: ask for it
    when you can name the wallet, never on anything derived per user.


42. **A drain is a debit, not a third kind of thing.** Every movement bsc records
    is now money arriving at a managed wallet or money leaving one. A drain is
    the second of those, with a reason attached and a link to the credits it
    carried.

    Before, a drain existed only as a `drain_tx` stamped on the deposits it
    swept: a movement of real money with no record of its own, discoverable only
    by finding something it had touched. A caller reconciling a wallet had to
    learn a third concept and a third way of finding it. Now `sum(credits) −
    sum(debits) == balanceOf`, and nothing is outside that.

    The drain's record is created **when the transfer is observed**, not when the
    flow starts, and is born terminal. That is the honest shape: a drain is not a
    promise. Nothing is owed to anybody while it is in flight, nobody can refuse
    it, and it never counts against the wallet — so there is no pending phase to
    represent. A payout is the opposite and keeps its pending phase, because
    somebody is waiting and the money must be held.

    Two consequences fall out of that asymmetry and are worth stating:

    - The sweep's amount is resolved at signing time from `balanceOf`, so the
      **sender writes it onto the flow** before broadcasting. Without that the
      settlement would know a transfer happened but not how much moved.
    - `/v1/withdrawals` becomes a **settled feed** with a cursor, mirroring
      `/v1/deposits`: both are observations of transfers that have happened, both
      resumable, both walked together to reconcile. Outstanding payouts are a
      different question — a promise has not happened yet — and are asked of a
      wallet (`/v1/wallets/{ref}/withdrawals`) or of an id.

    The line this must not cross: these are **observations of chain transfers**,
    not entries asserting ownership. `sum(credits) − sum(debits)` is a fact about
    an address. The moment something in bsc computes a net position *per person*
    from them, the ledger is back and §33 has been undone.

43. **A fee is a second debit, asked for in the same call.** `POST
    …/withdrawals` takes an optional `fee`, and bsc writes two records: the
    payout to where the caller said, and the fee to the wallet that pays for gas.

    This sits exactly on the line between business logic and primitive, so it is
    worth being precise about which side each part lands on. bsc does **not**
    decide what a fee is, how it is computed, who owes it, or whether one applies
    — the caller has already answered all of that and is passing a number. What
    bsc adds is three things it is uniquely placed to: the two are checked
    against the balance together, so a wallet can never accept the payout and
    refuse the fee; one idempotency key covers both; and the destination is
    resolved rather than named, because "the wallet that pays for gas" is not a
    policy choice.

    That last one closes a loop §38 opened. bsc spends native currency and earns
    nothing back, so the master drains monotonically and has to be topped up.
    Fees landing there are what `bsc swap` trades — without bsc having computed a
    fee or decided that one was due.

    **It is not atomic, and it cannot be.** Two ERC-20 transfers are two
    transactions; nothing short of a contract that performs both in one call
    makes them land together, and that would be a second trust root to deploy
    and maintain. So the payout can confirm while the fee is still retrying. The
    guarantee is joint *acceptance*, never joint *settlement*, and the records
    are two ordinary debits rather than one with two legs precisely so that the
    partial state is representable instead of being a status that needs repair —
    which is the mistake §24 retired the first time.

    The alternative considered was one record with two transfers and a status
    that meant "half done". It is the same `partial` state, and it would need the
    same manual repair.


44. **Fees came back; the automatic swap did not.** §38 removed the gas top-up
    for two reasons, and §43 voided the first of them — there are fees again,
    and they land on the master. The second reason stands on its own, and is why
    trading is still a command.

    A swap is the only operation whose outcome is a **price** rather than a yes
    or no. Everything else bsc does is deterministic: a transfer lands or it
    reverts. A swap can succeed and still be a bad outcome — a thin pool, a wide
    spread, a sandwich. Automating it means the service takes a price nobody
    looked at, at a moment chosen by a threshold crossing.

    That last part is the specific risk, and it is worth naming: an automatic
    top-up is **predictable**. The trigger is a public balance crossing a
    configured floor, the size is a configured constant, and the transaction
    goes through a public mempool. Anyone who wants to can watch for it and
    price accordingly. Attended trading is irregular in timing and size, which
    is not a guarantee but is a much poorer target. The amounts here are small
    enough that this is a bounded concern rather than an alarming one — but it
    is a concern that only exists if the trade is automated.

    And the urgency the automation was defending against does not survive
    arithmetic. The floor is a *warning* line, not empty: at 0.05 native and
    65k gas per transfer, the master has roughly 770 transfers of runway below
    it at 1 gwei, and still 150 at 5. Hitting the floor is a "this week" problem,
    not a "wake somebody" one — which is exactly the kind of thing a human
    should decide with a price in front of them.

    What is worth automating is the *checking*, and that is where it now lives:

        bsc swap --buy-bnb 0.1 --if-below --yes

    `--if-below` no-ops unless the master is under `gas.floor_wei`, so this is a
    crontab line. The automation is the operator's — readable, auditable,
    switch-off-able with a `#` — rather than a flow kind, five config settings
    and an autonomous price-taking decision inside a service whose whole claim
    is that it does not make business decisions.

    An unset floor reads as "fine", never as "always trade". Getting that
    backwards would mean trading on every run.


45. **No compatibility code, because there is nothing to be compatible with.**
    Nothing has been deployed and no database exists from a previous shape, so
    everything written to tolerate one is deleted rather than carried.

    Concretely: the enum values are densely numbered again, with no gaps
    reserved around the flow kinds and states that §33 and §38 removed; the
    audit no longer distinguishes "a reason a newer binary wrote" from "a reason
    nobody wrote", because a rollback to a binary that predates this shape is
    not a thing that can happen; and the schema check refuses a version it does
    not recognise outright rather than reserving a path to migrate one forward.

    The record version byte stays, and is the one deliberate exception. It is
    not compatibility with the past — there is none — it is the ability to
    append a field later without a migration, and it is the only part of this
    that cannot be bought retroactively: giving existing records a version byte
    is itself the migration it would have saved. One byte per record, paid now,
    while it is free.

    This entry exists so that the next person to add a gap or a legacy branch
    has to say what data it is for.


46. **One transfer flow, not two.** `FlowDrain` and `FlowWithdrawal` ran the same
    state machine — same states, same transitions, same terminal handling — and
    differed only in whether the amount was carried on the flow or read from the
    chain at signing. That is a property of a value, not a reason for a type.

    They are now one `FlowTransfer`, with `StateSweeping` and `StatePaying`
    collapsed into `StateMoving` and `ActionSweep`/`ActionPay` into `ActionMove`.
    The sender has one `move` that reads its amount from the action: nil means
    everything the wallet holds, set means exactly that. This is the same move
    §42 made on the records — one mechanism, the reason attached rather than
    baked into it.

    The distinction did not disappear, it moved and got stronger. `Amount` and
    `Withdrawal` now travel together or not at all, enforced at `flow.Begin`, so
    settlement reads `f.Pays()` as a validated fact rather than switching on a
    kind that nothing checked against the flow's own contents. A half-specified
    transfer — an amount with no withdrawal, or a withdrawal with no amount —
    is refused at creation rather than discovered when it settles.

    Logs, metrics and the dashboard still say `drain` and `payout`, via
    `Flow.Label()`. Those are the words a debit's reason already uses, so one
    movement reads the same whether you are watching it happen or reading what
    happened.

    **This uncovered a bug that had been live since §42.** A drain's amount is
    resolved by the sender from a balance nobody else sees, and it was never
    written back to the flow — so every drain's debit would have recorded an
    amount of zero. The engine test passed because it set the field by hand.
    `move` now returns what it moved and `journalAndSend` persists it in the same
    transaction as the journal, and the assertion lives end-to-end in
    `cli/e2e_test.go`,
    which is the only level at which the gap is visible.


47. **Eighteen packages became fourteen.** A package should be a boundary
    somebody can name. Four were not, and merging them changed no behaviour:

    - `money` (86 lines) had two importers, and `store` used it for exactly one
      four-line helper. Parsing a decimal string is a *wire* concern, so it went
      to `api/money.go` and its functions stopped being exported — `store` now
      carries its own `orZero` and imports nothing.
    - `ui` (38 lines) existed to hold a `//go:embed` and one handler, and `api`
      was its only importer while `api/ui.go` already served the state the page
      renders. The static files moved to `api/static`.
    - `engine` (311 lines) said it was separate "because two callers need it",
      which argues for a shared function rather than a package — `flow` has
      three callers and is not separate for that reason. `flow` already imported
      `store`, so its no-I/O property was never compiler-enforced; it is a
      convention that survives the merge exactly as well as it survived before.
      It is now `flow/engine.go` beside `flow/rules.go`.
    - `app` (434 lines) was wiring with one importer, `cli`. `app.Run` became
      `runService`; `Verify`/`VerifyOnChain` stay exported because the e2e suite
      imports them.

    **`swap` was on the list and stayed.** It has two importers and looks like a
    `cli` detail — but `e2e/swap_test.go` proves the router calldata against a
    real router, and it can only do that if the encoding is an importable
    package. The e2e suite is the only thing that can catch a wrong `Pack*`, so
    it decides where this code lives. Moving its two router addresses elsewhere
    was rejected for the same reason the merge was: an address belongs with the
    code that speaks to it, which is why USDT's live in `usdt` and the router's
    live in `swap`.

    `keys` was raised too and stays, for the opposite reason: it has four
    importers and `sender` has one, so folding it in would make `api` and `cli`
    depend on the signing package and dilute the one claim that directory makes.

## Verification

Every test runs offline — no chain, no network, no database server. The store is
a temp file, HTTP is `httptest`, and `cli/chainsim_test.go` is a miniature chain
that actually *applies* transactions: an `approve` sets a real allowance, a
`transferFrom` checks it and the balance exactly as the token would, moves the
tokens, and emits the `Transfer` log the watcher then decodes. That is what lets
the end-to-end tests walk a deposit all the way to a payout and assert against
on-chain balances rather than against our own records.
