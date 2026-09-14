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
  apps (HTTP) ─────▶│  │  bbolt: state/ · data/ · log/        │    │
                    │  └──────────────────────────────────────┘    │
                    │      ▲                      ▲                │
                    │     api                 snapshotter          │
                    └──────────────────────────────────────────────┘
```

| Role | Does | Driven by |
| --- | --- | --- |
| watcher | finalized head → block receipts → match → **one write transaction** per block | poll interval |
| sender | claims the work a flow's state owes: signs, journals, broadcasts | a nudge, or a tick |
| api | the HTTP surface plus `/metrics` and `/healthz` | requests |
| snapshotter | periodic consistent backups | interval |

## The store is the design

Everything is one bbolt file in three namespaces:

```
state/   cursor · flows · in-flight tx index · send journal        self-pruning
data/    apps (with their ledgers) · wallets · their indexes       catastrophic to lose
log/     deposits · withdrawals — the ledger's entries             kept forever
```

Records are hand-packed binary — a 20-byte address is 20 bytes, an amount is its
native 32-byte big-endian form — each with a leading version byte, so a new field
is an append rather than a migration. Sorted keys do real work: the deposit
cursor *is* `<block><logindex>`, the send journal is nonce-ordered per signer,
and deposit dedup is a property of the key rather than a constraint to check.

Having one store is what makes a finalized block atomic. Applying one commits, in
a single transaction: confirmations for transactions that landed, balance credits
and debits, deposit records, any new work the rules imply, and the cursor. There
is no second system to keep in step, so there is nothing to reconcile afterwards
and no dedup window to tune.

The cost is hand-rolled secondary indexes, maintained inside the same transaction
as the record they index. `bsc inspect` exists to catch exactly that class of bug.

## Pipelines: sequential execution, persisted waiting

Every piece of chain work is a **flow**: a persisted state machine.

| Flow | States (skipping to the last if the wallet is already active) |
| --- | --- |
| `drain` | `funding` → `approving` → `sweeping` → `done` \| `failed` |
| `withdrawal` | `funding` → `approving` → `paying` → `done` \| `failed` |
| `house_sweep` | `funding` → `approving` → `sweeping_house` → `done` \| `failed` |
| `gas_topup` | `approving_router` → `swapping` → `done` \| `failed` |
| `prewarm` | `funding` → `approving` → `done` |

These are **internal** and no app ever sees one (§27). The overlap with the
app-facing vocabulary is accidental and worth keeping straight: a flow's `failed`
is one attempt giving up, which for a withdrawal means a backoff and a retry, not
a verdict — a *withdrawal* has no failed status at all (§28).

Activation is not a separate flow — funding and approving are simply the prefix
of whatever needed an inactive wallet.

`sweeping` and `sweeping_house` are distinct states for the same reason the
wallets are distinct. `sweeping` moves a deposit wallet's *whole* balance, which
is only ever correct there: everything on a deposit address is owed to the app.
`sweeping_house` draws on the top-level wallet, which holds the app's money
alongside the house's, so it moves a computed difference — `balanceOf` less the
ledger, resolved at signing time — and never the balance (§25).

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

> a **deposit** wallet with `balance ≥ DRAIN_THRESHOLD_WEI`, no live flow, and past its retry deadline is owed a **drain**
> an app with a pending withdrawal, an idle top-level, and past its retry deadline is owed a **withdrawal**
> an app whose wallet holds more than its ledger, by at least `HOUSE_SWEEP_MIN_CENTS`, is owed a **house sweep**

Two deposits in one block are credited before the check runs, so the wallet is
evaluated once with the summed balance and exactly one drain starts. A deposit
landing *during* a drain finds the wallet busy and starts nothing — and the
evaluation after that drain terminates sees the leftover and drains it, so funds
cannot be stranded by arriving between the balance read and the signature. A
deposit landing during *activation* needs nothing at all, because the sweep reads
`balanceOf` when it signs.

Since the rules read current state rather than react to events, the startup pass
converges whatever was missed while the process was down.

## Money accounting

An app owns one top-level hot wallet; its deposit addresses drain into it. Two
quantities live side by side, in two units, and confusing them is the bug this
part of the design exists to prevent:

```
ledger  (cents)   what the app is owed     ← credited deposits − settled withdrawals
custody (wei)     what the wallet holds    ← every transfer the watcher observed
                  custody − ledger = the house's
```

The chain can say how much a wallet holds. It can never say whose it is: one hot
wallet carries the app's money, the fees we have charged and sub-cent dust in a
single number. So the app-facing balance is a **ledger**, and the wallet balance
means custody and nothing else (§22).

- `available` = ledger − reserved — what the app may spend now
- `reserved` — held by pending withdrawals, payout and fee together
- `pending` — recorded deposits whose drain has not landed: real, not yet
  spendable, and counted from the deposit records rather than from what the
  deposit wallets hold, because those also carry dust nobody was credited for
- `total` — the three added up, so an app never has to work out which pair to add

The three parts partition the ledger, which is what lets a deposit's status name
the bucket it is in: a `pending` deposit is what `pending` counts, a `credited`
one is in `available` (§27).

The ledger is **materialised for reads and recomputable from `log/`**, which is
the property the old chain-materialised balance could not offer: `bsc inspect`
rebuilds it from credited deposits and settled withdrawals and compares. It also
checks the inequality that matters — custody ≥ ledger, per app — which is the
first time this service has been able to assert that it is solvent.

### Where the fee goes

A withdrawal takes **one** reservation covering payout and fee, because both
leave the ledger at the same moment. Only the payout moves on-chain. The fee is
collected by *not* crediting it: it stays in the hot wallet, above the ledger,
and is therefore ours (§24). A withdrawal is one transfer, there is no second leg
to fail, and there is no `partial` status.

What is left over — fees, the sub-cent remainders flooring leaves behind, and
anything a stranger sends to a managed address — is one quantity after the
ledger, and one rule collects it: the **house sweep**, resolved at signing time
from `balanceOf` less the ledger, deferring to any pending payout (§25).

## Nothing escapes us, with one exception

Every payable address is derived by us, and every USDT `Transfer` is a log with
`from`, `to` and `value`, so deposits, drains, payouts and sweeps are all
observed. That is what keeps custody honest; the ledger is then derived from the
subset of those transfers that credit somebody. USDT on BSC has no rebase, no fee-on-transfer and no balance-mutating
admin hook, so the sum of observed transfers *is* the balance. Our own gas spend
follows from our own receipts.

The exception is **native BNB arriving at the master** — an operator's manual gas
top-up is a plain value transfer, which emits no log at all, and a receipt
carries no `value` or `to` field to fall back on. That is a single
`eth_getBalance` gauge, not reconciliation, and it is the number that matters
most operationally: a dry master stops every pipeline.

There is therefore no reconciler process. Correctness is checked where it counts:
`balanceOf` immediately before spending, and `bsc inspect` on demand — which
since the ledger can check the balances too, not merely the indexes.

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
12. **A slug identifies an app; no API keys.** Loopback-only, first-party
    callers. Registration is public and idempotent; apps configure themselves.
13. **No admin API and no UI.** With slug addressing the app API already is the
    operator's read surface. Write-side operator actions did not survive
    scrutiny: master BNB is a gauge, retrying a genuinely failed transfer cannot
    succeed, and pausing a first-party app is something the app can do itself.
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
19. **Gas top-ups swap collected fees back into native currency**, reversing v1's
    decision that gas replenishment stays manual. That decision reasoned the
    operator holds the key and can swap by hand — true, but it weighed an
    attended service. The top-up is a flow like any other, so it inherits the
    journal, the sequential nonce lane and the audit; it is off unless a router
    is configured; and it is bounded by a cooldown, because a swap that succeeds
    without lifting the balance above the floor would otherwise trade away every
    fee. It is the only operation whose outcome is a price rather than a yes or
    no, so it is also the only one with a slippage bound and a deadline.
20. **The swap uses the Uniswap-V2 router interface**, not a Universal Router,
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
22. **An app's balance is an internal ledger, not a chain balance.** The chain can
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
23. **Cents on the wire; wei only where we touch the chain.** Every amount an app
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
    `DRAIN_THRESHOLD_WEI`, decides only whether moving the money is worth the
    gas — the app is credited either way, and sees the difference as `pending`.
24. **A withdrawal is one transfer again, and the fee is collected by not paying
    it.** The fee leaves the app's ledger with the payout but never moves on its
    own: it stays in the hot wallet, now belonging to the house. This supersedes
    the sequential payout→fee flow, which cost a second transaction and a second
    settlement path per withdrawal, and it deletes the accrual namespace, the
    fee reservation and the `collecting` state along with it. Payout and fee now
    share one reservation because they share one lifetime.
25. **One rule collects everything nobody is owed.** After the ledger, the fees we
    charged, the sub-cent remainders flooring left behind, and any tokens a
    stranger sent to a managed address are the same quantity: the excess of
    custody over the ledger. A single `house_sweep` flow moves it to the
    collector once it is worth a transfer, and defers to any pending payout.

    "Everything" is the top-level wallet's excess, which is narrower than it
    sounds: the sweep is scoped to `KindTopLevel` and the drain to `KindDeposit`
    above `DRAIN_THRESHOLD_WEI`, so a deposit address that only ever receives
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
    the default was mainnet, so pointing `RPC_URL` at testnet and forgetting it
    produced a service that synced blocks, recorded deposits correctly, and had
    every single transaction rejected: the signer binds each one to the
    configured id (EIP-155), so the node refuses them all. Reads work, writes
    fail, and nothing in the logs says why.

    Now `RPC_URL` is the one chain input. `eth_chainId` after connecting decides
    the token, the router and the signer, so there is no second value left to
    disagree. `Config.Load` still does no I/O and still refuses a malformed
    address before anything dials; `Config.ResolveChain` fills in what only the
    chain can answer. An explicit `TOKEN_ADDRESS` or `SWAP_ROUTER` still wins,
    and an endpoint we have no defaults for is a startup error naming exactly
    what is missing.

    The chain id joins the token and its decimals in `data/`, checked on every
    later start. A database built against one chain cannot be opened against
    another — the wallets in it were derived for that chain and the amounts are
    denominated in its token, so moving it is a migration, not a config edit.

27. **The app-facing API is a payments provider's, not a chain's.** An app asks
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
    `TOKEN_ADDRESS` is configurable, the posture is to retry and make it visible
    rather than to guess — `attempts` climbing into double digits is an
    operator's signal, not a retry's, and the remedy today is editing the
    record. A cancel is the obvious follow-up and is deliberately unbuilt
    (`docs/ROADMAP.md`): releasing a reservation for a payout that might still
    land is the one way this design could pay twice.


## Verification

Every test runs offline — no chain, no network, no database server. The store is
a temp file, HTTP is `httptest`, and `app/chainsim_test.go` is a miniature chain
that actually *applies* transactions: an `approve` sets a real allowance, a
`transferFrom` checks it and the balance exactly as the token would, moves the
tokens, and emits the `Transfer` log the watcher then decodes. That is what lets
the end-to-end tests walk a deposit all the way to a payout and assert against
on-chain balances rather than against our own records.
