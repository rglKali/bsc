# Operating bsc

Day-2 guide for whoever runs it. For installation see
[`../deploy/README.md`](../deploy/README.md); for the API that internal services
call see [`CONSUMING.md`](CONSUMING.md); for how it works see
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## Mental model

One process, one file. It follows finalized BSC blocks, sweeps whatever lands on
the addresses it derived, and pays out when an app asks. It holds the master
secret, pays all the gas, and exposes an HTTP API on loopback.

**Lose `BSC_MASTER_SECRET` and you lose every app's funds.** It signs everything and
holds an unlimited allowance on every wallet the service derives. Protecting it
is the whole security model — there are no API keys, and reachability is the only
access control.

On the box it lives in `/etc/bsc/bsc.env` (0640 root:bsc), which the unit loads
and which holds that one value. Everything else is in `/etc/bsc/config.yaml`,
which needs no such protection — that separation is the point (§30).

## Day to day

The startup line names the chain it detected, the token and the cent scale — the
first thing to check if it is talking to the wrong network:

```sh
journalctl -u bsc | grep "bsc starting"
```

```sh
systemctl status bsc
journalctl -u bsc -f              # structured JSON; BSC_LOG_LEVEL=debug for more
curl -s localhost:8800/healthz | jq
curl -s localhost:8800/metrics | grep '^bsc_'
```

`/healthz` answers `200 {"status":"ok"}` or
`503 {"status":"degraded","degraded":[…]}`, and it checks the three things that
leave the service answering requests while unable to do its job:

- **the database will not answer** — nothing can be read or written;
- **the chain sync is past `chain.max_lag_blocks`** — withdrawals are already
  being refused, because balances are no longer current;
- **the master is below `swap.gas_floor_wei` *and* cannot refill itself** —
  either swapping is off, or it holds less than `swap.amount_wei` to sell.

That last condition is the one worth understanding. A low master on its own is
**not** reported, because it is the normal self-healing case: the top-up sells
collected fees back into gas without being asked. It is only a human's problem
when it is low *and* there is nothing left to sell — the same combination the
Grafana master panel is built around.

> **Wire this to monitoring, not to a restart.** A `503` here means "stop
> sending traffic and look at me", never "bounce me". Nothing it reports is
> fixed by starting the process again, and a restart mid-sync makes the lag
> worse.

Since there is no admin API, the app API *is* your read surface — any local
caller can read any app:

```sh
curl -s localhost:8800/v1/apps/df | jq
curl -s localhost:8800/v1/apps/df/withdrawals | jq   # still pending
```

Locally, `task sandbox` runs the service against Chapel with the dashboard on
and its own database, which is the intended way to click around without going
near real money.

With `ui_enabled: true` (or `BSC_UI_ENABLED=true`) there is a dashboard at `/ui/` covering the same ground
plus what `curl` cannot reach: custody beside the ledger, the excess and whether
it has gone negative, and the live flow list — which is the fastest answer to
"why has nothing moved for ten minutes". It is off by default and should stay off
anywhere the listener is not yours alone: it has no authentication and it reaches
every app, so it is strictly more dangerous than a single app's slug (§29).

## What to watch

| Metric | Why |
| --- | --- |
| `bsc_master_bnb_wei` | A dry master stops every pipeline: no activation, no drain, no payout. Gas top-ups defend this automatically (see **Gas**), so what deserves an alert is it staying low — that means the converter could not fix it. |
| `bsc_master_usdt_wei` | The fees the master has collected, and what a gas top-up has to sell. If this is flat at zero while `bsc_master_bnb_wei` falls, the converter has nothing to work with and you must top up by hand. |
| `bsc_blocks_behind` | At ~2.2 blocks/s lag accumulates fast. Sustained growth means the RPC cannot keep up. |
| `bsc_in_flight_age_seconds` | Signing is sequential, so one wedged transaction blocks everything behind it. Growth here is the stuck-transaction signal. |
| `bsc_rpc_wait_seconds` | Time blocked on the rate limiter. Rising means `chain.rpc_rate_limit` is the bottleneck. |
| `bsc_solvency_shortfalls_total` | **Any value above zero is serious.** A hot wallet holds less than its app's ledger says it is owed. Nothing self-corrects this; stop and understand it. |
| `bsc_balance_underflows_total` | Any value above zero is a bug in our own accounting. Investigate rather than restart. |
| `bsc_insufficient_balance_total` | A payout was refused at signing because the chain held less than our records. Means custody drifted from what we recorded. |
| `bsc_rpc_errors_total` | Provider health. |
| `bsc_flows_started_total`, `bsc_transactions_sent_total` | Throughput, by kind. |

`bsc_transactions_in_flight` is 0 or 1 by design — it is not a throughput gauge.

## Gas

The master pays BNB for every funding, approval, drain and payout, and it **tops
itself up**: when its native balance falls below `swap.gas_floor_wei` (0.05 by
default) and it holds at least `swap.amount_wei` of collected fees, it trades
that amount for native currency through `swap.router`. The fees the withdrawals
earned pay for the gas the withdrawals cost.

This is the only work the service starts on its own initiative, so it is hedged
on every side: it needs a configured router, a balance under the floor, enough
tokens to trade, an idle master, and `swap.cooldown` (1h) since the last attempt.
The cooldown matters most — a swap that succeeds but does not lift the balance
above the floor, because the floor is too high or the trade too small, would
otherwise re-fire immediately and keep trading fees away until there were none
left. `swap.slippage_bps` (1%) bounds the price; the wrapped-native address comes
from the router itself rather than configuration, so the two cannot disagree.

**It is not a substitute for watching the balance.** The converter can only sell
fees it actually holds, so a quiet stretch with no withdrawals, a `money.fee_collector`
pointed somewhere other than the master, or `swap.enabled=false` all leave a
draining master with nothing to sell. Send BNB by hand when `bsc_master_bnb_wei`
falls and no fees are accruing behind it; the address is in the startup log line.

Native transfers *into* the master emit no log, which is exactly why that number
is polled rather than derived — it is the only quantity in the system the service
cannot compute for itself.

Each deposit address is funded once, for its own `approve`, and never needs gas
again. Unused gas is refunded to the sender, so a few cents of BNB dust stays in
each one permanently — reclaiming it would cost more than it is worth, which is
why `gas.funding_multiplier` should stay modest.

## The house's money

An app's balance is a **ledger in cents** — credited deposits less settled
withdrawals — and the wallet it draws on holds more than that: the fees you have
charged, the sub-cent remainders left by rounding deposits down, and anything a
stranger sent to a managed address. That excess is yours, and one rule collects
it.

```sh
curl -s localhost:8800/v1/apps/df | jq .balance   # what the app is owed
bsc inspect <snapshot.db>                         # `owed` totals every app's ledger
```

A `house_sweep` flow starts once an app's wallet holds at least
`money.house_sweep_min_cents` more than its ledger, and sends the difference to
`money.fee_collector`. It waits for any pending payout first, and its amount is computed
from `balanceOf` at signing time rather than fixed in advance — so a sweep never
takes money an app is owed, even if a drain lands while it is in flight.

Setting `money.house_sweep_min_cents: 0` turns collection off entirely. That is safe:
the excess is still yours, it simply accumulates in the app wallets until you
turn it back on.

## Backups

Everything is in one bbolt file, and the service snapshots itself when
`snapshot.dir` is set — a consistent copy of the whole database, taken from
inside a read transaction, no external tooling involved.

```sh
ls -lh /var/backups/bsc/
bsc inspect /var/backups/bsc/bsc-20260911T120000Z.db
```

Snapshots are written in the clear. The database holds no secret that is not
already on the machine — the master secret never enters it — so encryption
belongs to whatever ships them off the box, not here.

**Back up snapshots off-machine.** Losing the file loses the wallet UUIDs, and
without those, every address ever derived is unrecoverable even though you still
hold the master secret.

## Auditing

```sh
bsc inspect <snapshot.db>          # offline: reserves and every index
bsc inspect --rpc <snapshot.db>    # also compares every balance against the chain
```

It exits non-zero on findings, so it is safe to put in cron.

The offline pass:

- **recomputes every ledger from the log** — credited deposits less settled
  withdrawals — and compares it against the materialised figure (`ledger`
  findings). This is possible only because the balance is our own bookkeeping;
  the chain-derived balance it replaced could not be recomputed at all.
- **checks solvency**: each app's hot wallet must hold at least what its ledger
  says it is owed (`solvency` findings). The difference is the house's.
- **checks both directions of every index** — the class of bug hand-rolled
  indexes invite (`index`, `ownership` findings).

What it still cannot do offline is check our record of custody against the token
itself; that is what `--rpc` adds. A finding of kind `balance` there means the
watcher's view of a transfer diverged from what the token actually did — serious,
and worth stopping to understand rather than restarting through.

Note that bbolt allows a single writer: the live database cannot be read while
the service holds it, which is why inspection works on snapshots.

## When something is wrong

**Everything has stopped.** Check `bsc_master_bnb_wei` first — a dry master is by
far the most common cause, and if it is dry *despite* top-ups being enabled, the
master had no fees to sell or the swap kept failing (`bsc_flows_started_total`
by kind, and the flow's `error`). Then `bsc_blocks_behind`: the watcher is what observes
finality, so if it has stalled, every in-flight transfer waits with it.

**One transaction is wedged.** `bsc_in_flight_age_seconds` climbing means a
signed transaction has not landed. The service re-broadcasts the same bytes on a
timer and will not abandon or re-sign it — doing so could double-spend if the
original later mines. If it stays stuck, it usually means its nonce was consumed
by a transaction you signed by hand; that needs your judgment, not a restart.

**A withdrawal is stuck.** There is no failure state: a payout that reverts
keeps its reservation, stays `pending`, and is retried behind a growing backoff
(§28). Nothing is charged until it lands, so the books are never wrong — but the
app's money stays reserved while this goes on, and nothing resolves it by itself.

`attempts` on the withdrawal is the signal. A handful is a transient problem
sorting itself out, which is the usual case: the token moves balances without
consulting the destination, so a payout fails on *our* side — a hot wallet
momentarily short, an allowance not yet in place, a node that refused the
broadcast — and the next attempt succeeds.

Double digits means something retrying will not fix. Look at `last_error` first;
if it is a revert with no obvious cause, check whether the configured token has a
transfer blacklist covering that destination (the BSC default does not; mainnet
Tether does). The remedy is editing the record, and there is deliberately no
cancel — releasing a reservation for a payout that might still land is the one
way this design could pay twice.

```sh
curl -s localhost:8800/v1/apps/df/withdrawals | jq '.withdrawals[]
  | select(.attempts > 5) | {id, attempts, last_error}'
```

**A solvency finding, or `bsc_solvency_shortfalls_total` above zero.** A wallet
holds less than the app it belongs to is owed. The house sweep refuses to run
while this is true, which is deliberate. Do not restart through it: compare
`bsc inspect --rpc` against the chain, and check whether tokens were moved out of
a managed wallet by something other than this service.

**Apps are getting 503s.** The watcher is more than `chain.max_lag_blocks` behind.
Balances are not current, so payouts are refused rather than reserved against a
stale balance. Reads keep working. It clears itself once sync catches up; if it
does not, the RPC endpoint or `chain.rpc_rate_limit` is the problem.

**Restarting is safe.** Every signed transaction is journalled before it is
broadcast, so a restart re-sends the same bytes rather than signing a new one.
The work rules re-evaluate at startup and converge on whatever was missed, so
nothing needs to be replayed by hand.

## Upgrades

```sh
GOOS=linux GOARCH=amd64 task binary
sudo cp bin/bsc /opt/bsc/bin/
sudo systemctl restart bsc
```

Take a snapshot first if the release changes storage. Records carry a version
byte, so a new field is backward-compatible; the schema version is refused
outright if the file was written by a newer binary than the one starting.
