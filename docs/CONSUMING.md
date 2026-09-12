# Using bsc

You register an app, hand its users deposit addresses, and pay out. bsc holds the
keys, pays the gas, and collects what lands on the addresses it derived.

> **Every amount is in cents**, as a decimal string: `"150"` is $1.50. Never a
> decimal point, never a JSON number, never wei. Fields are named `*_cents` so
> there is nothing to guess. bsc converts to the token's own units internally,
> reading the scale from the contract. Everything on-chain is **finalized-only**.

## Concepts

| Term | What it is |
| --- | --- |
| **App** | You. A slug you choose (`df`, `lkr:acme`) that owns one hot wallet. |
| **Deposit address** | An address derived for one of *your* handles (`ref`), served under `…/wallets` because it is a wallet bsc derives and manages for you. Whatever lands on it is swept into your hot wallet. |
| **Drain** | That automatic sweep. You never ask for it. |
| **Withdrawal** | USDT from your hot wallet to an external address, plus your fee. |
| **Balance** | A ledger in cents: what bsc owes you. Not the same number as your hot wallet's chain balance. |
| **Master** | The wallet bsc signs with and pays all gas from. You never see it. |

There is **no authentication**: bsc listens on loopback and every caller is a
first-party service on the same box. Your slug in the path is your identity.

## Quick start

```bash
BSC=http://127.0.0.1:8800

# 1. register (idempotent — call it on every boot)
curl -X PUT $BSC/v1/apps/df
# → {"slug":"df","address":"0x9d67…","fee":{"flat_cents":"100",…},…}

# 2. an address for your customer
curl -X POST $BSC/v1/apps/df/wallets -d '{"ref":"cust-1"}'
# → {"ref":"cust-1","address":"0x9Ab4…","created_at":"…"}

# 3. give 0x9Ab4… to the customer. What arrives is swept into your hot wallet.

# 4. see what arrived
curl "$BSC/v1/apps/df/deposits?since="
# → {"deposits":[{"cursor":"48210577-9","ref":"cust-1","amount_cents":"5000",
#                 "amount_wei":"50007000000000000000","status":"credited",…}],
#    "cursor":"48210577-9"}

# 5. pay someone
curl -X POST $BSC/v1/apps/df/withdrawals \
  -d '{"destination":"0xRecipient…","amount_cents":"500"}'
# → {"id":"…","status":"queued","payout_cents":"500","fee_cents":"100"}

# 6. poll until it settles
curl $BSC/v1/apps/df/withdrawals?status=open
```

## Learning what happened

Nothing is pushed to you. The two things you poll for are shaped differently, and
only one of them needs a cursor.

**Deposits are unsolicited** — you cannot know one is coming — so you ask "what
is new since I last looked":

```
GET /v1/apps/df/deposits?since=48210577-9&limit=100
→ { "deposits": [ … ], "cursor": "48210603-2" }
```

The cursor is the chain's own ordering, `(block, log_index)`. It sorts like the
chain and is verifiable against a block explorer, but **treat it as opaque**:
pass it back, compare it, don't parse it. Store it *after* you have processed the
page, never before. Delivery is at-least-once and ordered, so key your handling
on `cursor` (equivalently `tx_hash` + `log_index`).

When nothing is new, the same cursor comes back — so an app that keeps passing it
never loses its place, however long it was down. Deposits are kept forever; there
is no replay window to fall outside.

**Withdrawals are yours** — you minted the id — so you poll your own open set:

```
GET /v1/apps/df/withdrawals?status=open
```

Each carries `status`, `tx_hash`, `payout_cents` and `fee_cents`. Drop each one
as it reaches `done` or `failed`. The set is bounded by how many you have in
flight.

A deposit's `status` tells you whether it is spendable yet: `confirmed` means
seen and credited to `pending`, `credited` means swept into your hot wallet and
spendable. For spending, read `…/balance` — it is the authority, and it accounts
for what is reserved by payouts already in flight.

## The fee

A **USDT charge on your own users** — an exchange-style flat "$1.00 to withdraw".
It has nothing to do with the BNB bsc spends on gas, which is the same whether
your fee is one dollar or zero. You set it; there is no floor.

```bash
curl -X PUT $BSC/v1/apps/df -d '{"fee":{"flat_cents":"100","min_cents":"0"}}'
```

| `deduct_fee` | Destination receives | Your balance is debited |
| --- | --- | --- |
| `false` (default) | `amount_cents` | `amount_cents + fee_cents` |
| `true` | `amount_cents − fee_cents` | `amount_cents` |

`min_cents` rejects withdrawals below it, and is also the only brake on payout
spam — a thousand dust withdrawals is a thousand transactions of the master's gas.

Ask before you commit:

```bash
curl -X POST $BSC/v1/apps/df/withdrawals/quote -d '{"amount_cents":"500"}'
# → {"amount_cents":"500","fee_cents":"100","payout_cents":"500","debit_cents":"600"}
```

The fee leaves your balance with the payout, in one step. There is nothing to
wait for and nothing held back afterwards: a withdrawal is a single on-chain
transfer, and once it is `done` your balance already reflects both.

## API

```
PUT    /v1/apps/{slug}                        {fee?, paused?}   create or configure
GET    /v1/apps/{slug}                        config and balances
GET    /v1/apps/{slug}/balance                {available_cents, reserved_cents, pending_cents}

POST   /v1/apps/{slug}/wallets                 {ref}             idempotent on ref
GET    /v1/apps/{slug}/wallets                 ?after=&limit=
GET    /v1/apps/{slug}/wallets/{ref}

POST   /v1/apps/{slug}/withdrawals/quote      {amount_cents, deduct_fee}
POST   /v1/apps/{slug}/withdrawals            {destination, amount_cents, deduct_fee, idempotency_key?}
GET    /v1/apps/{slug}/withdrawals            ?status=open|done|failed|all&limit=
GET    /v1/apps/{slug}/withdrawals/{id}

GET    /v1/apps/{slug}/deposits               ?since=<cursor>&limit=
GET    /v1/apps/{slug}/deposits               ?status=confirmed    awaiting a drain

GET    /healthz    GET /metrics
```

Withdrawal statuses: `queued` → `pending` → `done` | `failed`. That is the whole
set; a withdrawal is one transfer and cannot half-succeed.

Errors are `{"error": "...", "code": "..."}` with a meaningful status:

| Status | When |
| --- | --- |
| `400` | malformed request, bad address, bad amount, unknown JSON field |
| `403` | the app has paused its own payouts |
| `404` | unknown app, ref, or withdrawal |
| `409` | an idempotency key reused with different parameters |
| `422` | below your minimum, fee ≥ amount, or not enough available balance |
| `503` | chain sync is behind; withdrawals are paused until it catches up |

## Things worth knowing

**Register on every boot.** `PUT /v1/apps/{slug}` is idempotent and returns the
same wallet. You store nothing but your slug.

**Deposit addresses are permanent.** One address per `ref`, forever — never
rotated, never reused for anyone else. `POST` with a known `ref` returns the
existing one.

**Use an idempotency key for withdrawals.** A retry after a timeout replays the
original instead of paying twice; the same key with different parameters is a
`409`.

**Amounts are rounded down to the cent, and bsc keeps the remainder.** If a
customer sends 10.007 USDT you are credited 1000 cents and the 0.007 stays with
the service. Sub-cent transfers are not recorded at all. This is why
`amount_cents` and `amount_wei` are both on every deposit: the first is what you
were credited, the second is what a block explorer will show you, and they differ
by less than a cent, always in the same direction.

**Your balance is a ledger, not the wallet's chain balance.** It is exactly
credited deposits minus settled withdrawals and fees. Looking up your hot wallet
on a block explorer will usually show *more* than your balance — that extra is
the fees you have been charged and the sub-cent remainders, which are the
service's, not yours. Use `…/balance`; the explorer is not the same question.

**The first deposit to a fresh address is slower.** It pays for a one-time
activation (funding the address with gas, then approving) before the sweep. Later
deposits drain in one step, and a deposit arriving mid-drain is picked up right
afterwards.

**`503` means wait.** While the chain watcher is catching up, balances are not
current, so accepting a payout could overdraw you. Reads keep working throughout.

## Cheat sheet

| I want to… | Do this |
| --- | --- |
| Onboard my service | `PUT /v1/apps/{slug}` |
| Give a customer an address | `POST /v1/apps/{slug}/wallets {ref}` |
| See what arrived | `GET /v1/apps/{slug}/deposits?since=<cursor>` |
| Know what I can spend | `GET /v1/apps/{slug}/balance` |
| Pay out | `POST /v1/apps/{slug}/withdrawals` |
| Check payouts in flight | `GET /v1/apps/{slug}/withdrawals?status=open` |
| Change my fee | `PUT /v1/apps/{slug} {"fee":{…}}` |

**Golden rules:** amounts are whole cents as strings; register idempotently;
store the deposit cursor after processing; use idempotency keys; `…/balance` is
the authority on what you can spend — not the chain.
