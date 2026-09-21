# Using bsc

You ask for a wallet, are told what lands on it, and ask for payouts from it.
bsc holds the keys, pays the gas, and moves what you tell it to move.

> **Every amount is the token's own base units**, as a decimal string. For
> 18-decimal USDT on BSC, `"1000000000000000000"` is one USDT. Never a decimal
> point, never a JSON number, never a scaled figure.

bsc keeps **no ledger**. It does not know who owes whom and has no opinion about
whose money is on a wallet — that is the job of whatever keeps your books. What
it knows is what the chain says, and it reports exactly that.

Everything it records is one of two things: money **arriving** at a wallet it
derived, or money **leaving** one. Nothing else exists, which means
`sum(deposits) − sum(withdrawals) == balance` for every wallet, with no third
category to account for.

## Concepts

| Term | What it is |
| --- | --- |
| **Wallet** | An address bsc derived for one of your handles (`ref`). Unique across the service. |
| **`drain_to`** | Where this wallet forwards to. Set means it forwards; unset means it accumulates. |
| **Deposit** | A credit: money that arrived on one of your wallets. Immutable. |
| **Withdrawal** | A debit: money that left one. A payout you asked for, the fee beside it, or a forward bsc made. |
| **Balance** | What the chain says the address holds. There is no other balance. |

There is **no authentication**: bsc listens on loopback and every caller is a
first-party service on the same box. There is also no tenancy — any caller can
read every wallet — so if more than one of your services uses it, **namespace
your refs yourself** (`acme:cust-1`, `globex:cust-1`).

## The one decision: forward or accumulate

Every wallet is one or the other, and `drain_to` is the whole of it.

```
drain_to set                          drain_to unset
────────────                          ──────────────
forwards everything it receives,      keeps what it receives
once the amount is worth the gas

no withdrawals may be paid from it    withdrawals are paid from it
```

They are mutually exclusive because they contradict each other: one empties the
wallet, the other spends from it, and both racing for the same funds is how a
payout reverts. A withdrawal from a forwarding wallet is refused with `422`.

`drain_to` may point anywhere — another wallet bsc manages, or an address it has
never heard of. When it points at a managed wallet, bsc checks the chain of
references and refuses a cycle: `A → B → A` would move the same money round and
round, succeeding every hop and spending your gas forever.

## Quick start

```bash
BSC=http://127.0.0.1:8800

# 1. a wallet that collects and pays out
curl -X PUT $BSC/v1/wallets/treasury -d '{}'
# → {"ref":"treasury","address":"0x9Ab4…","balance":"0",…}

# 2. a wallet per customer, forwarding into it — costs nothing to create
curl -X PUT $BSC/v1/wallets/acme:cust-1 \
  -d '{"drain_to":"0x9Ab4…"}'
# → {"ref":"acme:cust-1","address":"0x3Cd7…","drain_to":"0x9Ab4…",…}

# 3. give 0x3Cd7… to the customer

# 4. see what arrived, anywhere
curl "$BSC/v1/deposits?since="
# → {"deposits":[{"id":"0000000002dfd5d100000009","wallet":"acme:cust-1",
#                 "amount":"50000000000000000000","tx_hash":"0x7c…"}],
#    "cursor":"0000000002dfd5d100000009"}

# 5. pay someone out of the treasury
curl -X POST $BSC/v1/wallets/treasury/withdrawals \
  -d '{"to":"0xRecipient…","amount":"5000000000000000000"}'
# → {"id":"…","status":"pending","amount":"5000000000000000000"}

# 6. poll until it settles
curl $BSC/v1/withdrawals
```

## Wallets

```
GET /v1/wallets/treasury
→ { "ref":       "treasury",
    "address":   "0x9Ab4…",
    "balance":   "50000000000000000000",
    "committed": "5000000000000000000",
    "available": "45000000000000000000",
    "paused":    false,
    "created_at": "…" }
```

| Field | Meaning |
| --- | --- |
| `balance` | What the chain says this address holds. |
| `committed` | Promised by withdrawals that have not settled yet. |
| `available` | `balance − committed` — the most a new withdrawal may ask for. |
| `drain_to` | Present only when this wallet forwards. |

**This is custody, not a ledger.** If several of your users share a wallet, bsc
cannot tell you how much is whose — it has never been told. Keep that in your
own books.

Retarget or pause with `PATCH`:

```bash
curl -X PATCH $BSC/v1/wallets/acme:cust-1 -d '{"drain_to":"0xNewTreasury…"}'
curl -X PATCH $BSC/v1/wallets/acme:cust-1 -d '{"drain_to":""}'      # stop forwarding
curl -X PATCH $BSC/v1/wallets/treasury    -d '{"paused":true}'      # block payouts
```

A wallet with a pending withdrawal cannot start forwarding (`409`): let the
payout settle first.

## Deposits

**Deposits are unsolicited** — you cannot know one is coming — so you ask "what
is new since I last looked":

```
GET /v1/deposits?since=<cursor>&limit=100
→ { "deposits": [ … ], "cursor": "0000000002dfd60300000002" }
```

Each deposit is:

```json
{ "id":         "0000000002dfd5d100000009",
  "tx_hash":    "0x7c3f…",
  "wallet":     "acme:cust-1",
  "from":       "0x1234…",
  "amount":     "50000000000000000000",
  "created_at": "2026-09-16T09:51:08Z" }
```

`id` is unique and doubles as the cursor: pass the last one back as `since`. It
is **opaque but ordered** — compare two ids to know which came first, pass one
back to resume, never parse one. Key your handling on `id`; `tx_hash` is not
unique, since one transaction can pay several of your wallets at once.

Store the cursor *after* you have processed the page, never before. Delivery is
at-least-once and ordered. When nothing is new the same cursor comes back.
Deposits are kept forever; there is no replay window to fall outside.

**A deposit has no status.** It is recorded once, after it happened, and never
changes: this much landed here, at this block. That is the whole of it.

Money leaving the wallet again is a *separate* record — a debit — rather than a
change to the credit. Whether a given deposit's money is still sitting there is
not a question bsc answers per deposit, because a forward moves the wallet's
**balance**, not a chosen set of deposits. Ask the wallet what it holds.

**A forward is a withdrawal.** There is no separate "drain" object: when bsc
moves a wallet to its `drain_to`, that is a debit with `reason: "drain"` on
`/v1/withdrawals`.

**A forward into another managed wallet produces a credit at the far end.** The
arrival is a deposit like any other, and the two ends pair through the debit:
the drain's `tx_hash` **is** the downstream credit's `tx_hash`. Deduplicate on
that if you are counting money.

The feed is **global** — every deposit on every wallet. `GET
/v1/wallets/{ref}/deposits` is the per-wallet view.

## Withdrawals

```bash
curl -X POST $BSC/v1/wallets/treasury/withdrawals -d '{
  "to": "0xRecipient…",
  "amount": "5000000000000000000",
  "idempotency_key": "order-4417"
}'
# → {"payout": {"id":"…","reason":"payout","status":"pending","amount":"5000000000000000000"}}
```

Every debit carries a `reason`:

| `reason` | Meaning |
| --- | --- |
| `payout` | You asked for it. |
| `fee` | The charge beside a payout — see below. |
| `drain` | bsc forwarding a wallet to its `drain_to`. Never pending: recorded once it has happened. |

**A withdrawal has no failure state.** There are two statuses:

| `status` | Meaning |
| --- | --- |
| `pending` | Accepted. The amount is committed and the payout is on its way. |
| `confirmed` | The transfer reached finality. Terminal. |

That splits by whose problem it is. If a request cannot be honoured — malformed
or zero destination, non-positive amount, more than the wallet has available, a
forwarding wallet, a paused wallet — you get an **error at creation** and no
withdrawal is created. There is nothing to poll and nothing to clean up.

Once bsc has accepted one, it is bsc's job to land it. If a payout reverts it is
retried, not failed back to you, so you never have to write compensation logic
for a payout that half-happened. `attempts` and `last_error` are diagnostics, not
a verdict.

This is safe because a payout has almost nothing to fail *at*: the token moves
balances without consulting the destination, so any valid non-zero address can
receive. The zero address is rejected outright at creation, along with every
other bad input.

> **Worth an alert anyway:** `attempts` in double digits on one withdrawal means
> something is wrong that retrying will not fix, and its `committed` amount stays
> held while it spins. That is a human's problem, not a retry's.

### Watching debits

`/v1/withdrawals` is a **settled feed**, exactly like `/v1/deposits`: everything
that has left, in the order the chain settled it, resumable by cursor.

```
GET /v1/withdrawals?since=<cursor>&limit=100
→ { "withdrawals": [ … ], "cursor": "0000…" }
```

Pending payouts are deliberately absent — this is a record of what happened, and
a promise has not happened yet. Walk this feed beside `/v1/deposits` and you have
every movement on every wallet.

Drains and fees are included by default, because they are real movements and you
need them to reconcile. Narrow with `?reason=payout|fee|drain`.

For what a wallet still owes, ask the wallet; for one payout, ask by id:

```
GET /v1/wallets/{ref}/withdrawals              still pending (default)
GET /v1/wallets/{ref}/withdrawals?status=all   its whole history
GET /v1/withdrawals/{id}                       one, whatever its status
```

**Use an idempotency key.** A retry after a timeout replays the original instead
of paying twice; the same key with different parameters is a `409`. Keys are
global across the service — make yours unique.

## Fees

bsc does not decide what a fee is. You do — how it is computed, who owes it,
whether one applies at all. What you can do is ask for it in the same call:

```bash
curl -X POST $BSC/v1/wallets/treasury/withdrawals -d '{
  "to": "0xRecipient…",
  "amount": "5000000000000000000",
  "fee": "1000000000000000000"
}'
# → {"payout": {…}, "fee": {"reason":"fee","part_of":"<payout id>","to":"0xMaster…"}}
```

That writes **two debits**: the payout to where you said, and the fee to the
wallet bsc pays gas from. The fee names its payout in `part_of`.

What you get over making two calls yourself:

- **They are accepted together.** Both are checked against the balance in one
  shot, so you can never end up with the payout accepted and the fee refused.
- **One idempotency key covers both**, and a retry replays both.
- **You do not have to know where fees go.** That is the wallet that pays for
  every transfer bsc makes, which is also what keeps it funded.

What you do **not** get, and must design around:

> **They are not atomic.** Two token transfers are two transactions, and nothing
> short of a contract makes them land together. The payout can confirm while the
> fee is still retrying. bsc guarantees joint *acceptance*, never joint
> *settlement*.

They are two ordinary debits with their own ids and their own lifecycles, which
is deliberate: a half-settled pair is a state you can see and reason about,
rather than a status that needs repairing.

If you want the fee somewhere other than the gas wallet, omit `fee` and issue a
second withdrawal of your own.

## API

```
GET    /v1/wallets                      ?after=&limit=
PUT    /v1/wallets/{ref}                {drain_to?, prewarm?}    idempotent
GET    /v1/wallets/{ref}
PATCH  /v1/wallets/{ref}                {drain_to?, paused?}

GET    /v1/wallets/{ref}/deposits       ?limit=
POST   /v1/wallets/{ref}/withdrawals    {to, amount, fee?, idempotency_key?}
GET    /v1/wallets/{ref}/withdrawals    ?status=pending|confirmed|all&reason=&limit=

GET    /v1/deposits                     ?since=<cursor>&limit=          the credit feed
GET    /v1/withdrawals                  ?since=<cursor>&reason=&limit=  the settled-debit feed
GET    /v1/withdrawals/{id}

GET    /healthz    GET /metrics      (operator surface, not part of the contract)
```

Everything about one wallet hangs off its own path; the two feeds are the only
top-level collections, because they are the only things that are not about one
wallet.

Statuses, in full: **deposits have none** — they are facts, recorded after the
event. Withdrawals are `pending` → `confirmed`, because one is accepted before
it happens. Reasons are `payout`, `fee`, `drain`. That is the entire vocabulary.

Errors are `{"error": "...", "code": "..."}` with a meaningful status:

| Status | When |
| --- | --- |
| `400` | malformed request, bad address, bad amount, bad ref, unknown JSON field |
| `403` | the wallet has payouts paused |
| `404` | unknown wallet or withdrawal |
| `409` | a ref reused with a different `drain_to`; an idempotency key reused with different parameters; retargeting a wallet with a pending payout |
| `422` | more than the wallet has available; a payout from a forwarding wallet; a `drain_to` that would form a cycle or run too deep |
| `503` | bsc is catching up; withdrawals are paused until it does |

Every `4xx` here is a request that was **not** created. Retrying it unchanged
will fail the same way; fix the input instead.

## Things worth knowing

**Create wallets idempotently.** `PUT /v1/wallets/{ref}` with a known ref returns
the existing one — unless you also ask for a different `drain_to`, which is a
`409` rather than a silent disagreement about where the money goes.

**Addresses are permanent.** One address per `ref`, forever — never rotated,
never reused for anyone else.

**Nothing is rounded.** The amount on a deposit is the amount the chain reported,
to the last unit. There is no dust and no minimum: even a one-unit transfer is
recorded.

**`money.drain_threshold_wei` is not a minimum deposit.** It decides only whether
forwarding is worth the gas. Below it the money waits on the wallet and is
recorded all the same; later deposits push it over and carry the earlier ones
along.

**Creating a wallet costs nothing.** Deriving an address signs no transaction
and pays no gas, so handing one to every user who signs up is free even if most
of them never deposit. Make as many as you like.

**The first movement out of a wallet is slower.** Before bsc can move a wallet's
tokens it has to fund that address with a little native currency and have it
approve the master — two transactions, once per wallet, ever. That happens on
the first drain or the first payout, not at creation.

For a deposit this is invisible: the money is recorded the moment it arrives and
you are not waiting on the forward. For a payout it is three transactions of
latency on the *first* one from a given wallet. If that matters — a treasury you
are about to draw on — create it with `prewarm: true` and the activation happens
up front instead:

```bash
curl -X PUT $BSC/v1/wallets/treasury -d '{"prewarm":true}'
```

Ask for that only on a wallet you can name. Never on anything derived per user:
that is precisely the case lazy activation exists for.

**`503` means wait.** While bsc is catching up, balances are not current, so
accepting a payout could overdraw a wallet. Reads keep working throughout.

## Cheat sheet

| I want to… | Do this |
| --- | --- |
| Collect into one place | `PUT /v1/wallets/treasury` then per-user wallets with `drain_to` |
| Give a customer an address | `PUT /v1/wallets/{ref} {drain_to}` |
| See what arrived | `GET /v1/deposits?since=<cursor>` |
| See what left | `GET /v1/withdrawals?since=<cursor>` |
| Know what I can pay out | `GET /v1/wallets/{ref}` → `available` |
| Pay out | `POST /v1/wallets/{ref}/withdrawals` |
| Pay out and take a cut | the same, with `fee` |
| Check payouts in flight | `GET /v1/wallets/{ref}/withdrawals` |
| Stop forwarding | `PATCH /v1/wallets/{ref} {"drain_to":""}` |

**Golden rules:** amounts are base units as strings; namespace your refs; store
the deposit cursor after processing; use idempotency keys; `available` is the
authority on what a wallet can pay.
