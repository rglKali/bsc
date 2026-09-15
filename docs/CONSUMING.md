# Using bsc

You register an app, hand its users deposit addresses, and pay out. bsc holds the
keys, pays the gas, and collects what lands on the addresses it derived.

> **Every amount is in cents**, as a decimal string: `"150"` is $1.50. Never a
> decimal point, never a JSON number, never a token unit. Fields are named
> `*_cents` so there is nothing to guess.

There is nothing else to learn. You will not find a wei amount, a block height, a
log index or a gas setting anywhere in this document, because none of them appear
on the wire. bsc is a payments provider that happens to settle on a chain; how it
settles is its problem.

## Concepts

| Term | What it is |
| --- | --- |
| **App** | You. A slug you choose (`df`, `lkr:acme`) that owns one balance. |
| **Deposit address** | An address bsc derives for one of *your* handles (`ref`). Whatever lands on it becomes your money. |
| **Deposit** | Money that arrived on one of your addresses. You did not ask for it, so you poll for it. |
| **Withdrawal** | A payout from your balance to an external address, plus your fee. |
| **Balance** | What bsc owes you, in cents, split by what you can do with it right now. |

There is **no authentication**: bsc listens on loopback and every caller is a
first-party service on the same box. Your slug in the path is your identity.

## Quick start

```bash
BSC=http://127.0.0.1:8800

# 1. register (idempotent — call it on every boot)
curl -X PUT $BSC/v1/apps/df
# → {"slug":"df","balance":{"available_cents":"0",…},…}

# 2. an address for your customer
curl -X POST $BSC/v1/apps/df/addresses -d '{"ref":"cust-1"}'
# → {"ref":"cust-1","address":"0x9Ab4…","created_at":"…"}

# 3. give 0x9Ab4… to the customer

# 4. see what arrived
curl "$BSC/v1/apps/df/deposits?since="
# → {"deposits":[{"id":"0000000002dfd5d100000009","ref":"cust-1",
#                 "amount_cents":"5000","status":"credited",
#                 "tx_hash":"0x7c…"}],
#    "cursor":"0000000002dfd5d100000009"}

# 5. pay someone
curl -X POST $BSC/v1/apps/df/withdrawals \
  -d '{"destination":"0xRecipient…","amount_cents":"500"}'
# → {"id":"…","status":"pending","payout_cents":"500","fee_cents":"100"}

# 6. poll until it settles
curl $BSC/v1/apps/df/withdrawals
```

## Your balance

One call answers "where is my money", and the four numbers add up:

```
GET /v1/apps/df/balance
→ { "available_cents": "899",
    "reserved_cents":  "101",
    "pending_cents":   "250",
    "total_cents":    "1250" }
```

| Field | Meaning |
| --- | --- |
| `available_cents` | Spendable this second. This is the one to check before a payout. |
| `reserved_cents` | Committed to payouts that have not settled yet. |
| `pending_cents` | Arrived, not spendable yet — bsc is still moving it into place. |
| `total_cents` | The three added up: everything that is, or will be, yours. |

Every cent sits in exactly one of the first three, so you never have to work out
which pair to add. Money moves `pending → available` on its own, and
`available → reserved → gone` when you pay out.

**This is a ledger, not a chain balance.** Looking your address up on a block
explorer will usually show more, because the same address also holds bsc's fees.
That extra is not yours and is not counted here. `…/balance` is the authority.

## Deposits

**Deposits are unsolicited** — you cannot know one is coming — so you ask "what
is new since I last looked":

```
GET /v1/apps/df/deposits?since=<cursor>&limit=100
→ { "deposits": [ … ], "cursor": "0000000002dfd603000000002" }
```

Each deposit is:

```json
{ "id":           "0000000002dfd5d100000009",
  "tx_hash":      "0x7c3f…",
  "ref":          "cust-1",
  "from":         "0x1234…",
  "amount_cents": "5000",
  "status":       "pending" | "credited",
  "created_at":   "2026-09-14T09:51:08Z" }
```

`id` is unique and doubles as the cursor: pass the last one back as `since`. It
is **opaque but ordered** — compare two ids to know which came first, pass one
back to resume, never parse one. Key your handling on `id`; `tx_hash` is not
unique, since one transaction can pay several of your addresses at once.

Store the cursor *after* you have processed the page, never before. Delivery is
at-least-once and ordered. When nothing is new the same cursor comes back, so an
app that keeps passing it never loses its place however long it was down.
Deposits are kept forever; there is no replay window to fall outside.

Two statuses, and they line up exactly with your balance:

| `status` | Counted in | Meaning |
| --- | --- | --- |
| `pending` | `pending_cents` | Arrived and yours. bsc is still moving it into place. |
| `credited` | `available_cents` | Spendable. |

`GET /v1/apps/df/deposits?status=pending` lists just the ones not yet spendable.

`tx_hash` is there for one purpose: pasting into a block explorer when somebody
asks "did that really arrive". You never need it to reconcile — `amount_cents`
is what you were credited and `…/balance` is what you may spend.

## Withdrawals

```bash
curl -X POST $BSC/v1/apps/df/withdrawals -d '{
  "destination": "0xRecipient…",
  "amount_cents": "500",
  "idempotency_key": "order-4417"
}'
```

**A withdrawal has no failure state.** There are two statuses:

| `status` | Meaning |
| --- | --- |
| `pending` | Accepted. The money is reserved and the payout is on its way. |
| `debited` | The payout landed and your balance is charged. Terminal. |

That is deliberate, and it splits by whose problem it is. If a request cannot be
honoured — malformed or zero destination, under your minimum, fee larger than
the amount, not enough available balance, payouts paused — you get an **error at
creation** and no withdrawal is created. There is nothing to poll and nothing
to clean up.

Once bsc has accepted one, it is bsc's job to land it. If a payout reverts it is
retried, not failed back to you, so you never have to write compensation logic
for a payout that half-happened. `attempts` and `last_error` are on the record as
diagnostics, not a verdict.

This is safe because a payout has almost nothing to fail *at*: the token moves
balances without consulting the destination, so any valid non-zero address can
receive. What goes wrong is on our side and passes. The zero address is rejected
outright when you create the withdrawal, along with every other bad input.

> **Worth an alert anyway:** `attempts` in double digits on one withdrawal means
> something is wrong that retrying will not fix, and its `reserved_cents` stays
> held while it spins. That is a human's problem, not a retry's.

Poll your own outstanding set; you minted the ids, so there is no cursor:

```
GET /v1/apps/df/withdrawals                   the ones still pending (default)
GET /v1/apps/df/withdrawals?status=debited    settled
GET /v1/apps/df/withdrawals?status=all
GET /v1/apps/df/withdrawals/{id}
```

Drop each one as it reaches `debited`. The outstanding set is bounded by how many
you have in flight.

**Use an idempotency key.** A retry after a timeout replays the original instead
of paying twice; the same key with different parameters is a `409`.

## The fee

A **USDT charge on your own users** — an exchange-style flat "$1.00 to withdraw".
It has nothing to do with what bsc spends on gas. You set it; there is no floor.

```bash
curl -X PUT $BSC/v1/apps/df -d '{"fee":{"flat_cents":"100","min_cents":"0"}}'
```

| `deduct_fee` | Destination receives | Your balance is debited |
| --- | --- | --- |
| `false` (default) | `amount_cents` | `amount_cents + fee_cents` |
| `true` | `amount_cents − fee_cents` | `amount_cents` |

`min_cents` rejects withdrawals below it, and is also the brake on payout spam.

Ask before you commit:

```bash
curl -X POST $BSC/v1/apps/df/withdrawals/quote -d '{"amount_cents":"500"}'
# → {"amount_cents":"500","fee_cents":"100","payout_cents":"500","debit_cents":"600"}
```

The fee leaves your balance with the payout, in one step. Nothing is held back
afterwards: once a withdrawal is `debited`, your balance already reflects both.

## API

```
PUT    /v1/apps/{slug}                    {fee?, paused?}   create or configure
GET    /v1/apps/{slug}                    config and balance
GET    /v1/apps/{slug}/balance            {available, reserved, pending, total}_cents

POST   /v1/apps/{slug}/addresses          {ref}             idempotent on ref
GET    /v1/apps/{slug}/addresses          ?after=&limit=
GET    /v1/apps/{slug}/addresses/{ref}

GET    /v1/apps/{slug}/deposits           ?since=<cursor>&limit=
GET    /v1/apps/{slug}/deposits           ?status=pending   not yet spendable

POST   /v1/apps/{slug}/withdrawals/quote  {amount_cents, deduct_fee}
POST   /v1/apps/{slug}/withdrawals        {destination, amount_cents, deduct_fee, idempotency_key?}
GET    /v1/apps/{slug}/withdrawals        ?status=pending|debited|all&limit=
GET    /v1/apps/{slug}/withdrawals/{id}

GET    /healthz    GET /metrics      (operator surface, not part of the app contract)
```

Statuses, in full: deposits are `pending` → `credited`, withdrawals are
`pending` → `debited`. That is the entire vocabulary.

Errors are `{"error": "...", "code": "..."}` with a meaningful status:

| Status | When |
| --- | --- |
| `400` | malformed request, bad address, bad amount, unknown JSON field |
| `403` | the app has paused its own payouts |
| `404` | unknown app, ref, or withdrawal |
| `409` | an idempotency key reused with different parameters |
| `422` | below your minimum, fee ≥ amount, or not enough available balance |
| `503` | bsc is catching up; withdrawals are paused until it does |

Every `4xx` here is a request that was **not** created. Retrying it unchanged
will fail the same way; fix the input instead.

## Things worth knowing

**Register on every boot.** `PUT /v1/apps/{slug}` is idempotent. You store
nothing but your slug.

**Deposit addresses are permanent.** One address per `ref`, forever — never
rotated, never reused for anyone else. `POST` with a known `ref` returns the
existing one.

**Amounts are rounded down to the cent.** If a customer sends 10.007 USDT you are
credited 1000 cents. Anything worth less than a whole cent is not recorded at
all. The remainder is bsc's, which is what the fee model is built on — you never
see it and never need to account for it.

**The first deposit to a fresh address is slower.** A new address needs one-time
setup before its money can be moved. Later deposits are quicker. Either way the
deposit is recorded — and counted in `pending_cents` — the moment it arrives, so
you can credit your user immediately if you choose to.

**`503` means wait.** While bsc is catching up, balances are not current, so
accepting a payout could overdraw you. Reads keep working throughout.

## Cheat sheet

| I want to… | Do this |
| --- | --- |
| Onboard my service | `PUT /v1/apps/{slug}` |
| Give a customer an address | `POST /v1/apps/{slug}/addresses {ref}` |
| See what arrived | `GET /v1/apps/{slug}/deposits?since=<cursor>` |
| Know what I can spend | `GET /v1/apps/{slug}/balance` → `available_cents` |
| Pay out | `POST /v1/apps/{slug}/withdrawals` |
| Check payouts in flight | `GET /v1/apps/{slug}/withdrawals` |
| Change my fee | `PUT /v1/apps/{slug} {"fee":{…}}` |

**Golden rules:** amounts are whole cents as strings; register idempotently;
store the deposit cursor after processing; use idempotency keys; `available_cents`
is the authority on what you can spend — not the chain.
