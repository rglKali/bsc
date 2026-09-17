# Operating bsc

Day-2 guide: what the service does on its own, what it needs from you, and what
to do when something looks wrong.

## Mental model

One process, one file. The process follows finalized BSC blocks, records what
lands on the addresses it derived, forwards the wallets configured to forward,
and signs the payouts its caller asks for. Everything it knows lives in one bbolt
database, and a finalized block is applied as a single transaction — so there is
no second system to keep in step and nothing to reconcile.

Three things are yours, not the service's:

1. **Gas.** bsc spends native currency on every transfer and earns nothing back.
   Nothing refills the master automatically; that was removed in §38 along with
   the fee that used to fund it.
2. **The books.** bsc keeps no ledger and has no idea whose money is on a wallet.
   Whatever answers that question runs above it.
3. **Backups.** Losing the database loses the wallet ids, and without them every
   address ever derived is unrecoverable even though you still hold the master
   secret.

## Day to day

Normally: nothing. The service converges on its own, and the rules re-evaluate
after every block, so a restart picks up whatever was missed while it was down.

```sh
systemctl status bsc
journalctl -u bsc -f
curl -s localhost:8800/healthz | jq .
```

`/healthz` reports three conditions that leave the process answering requests
while unable to do its job: a store that will not answer, a sync far enough
behind that withdrawals are being refused, and a master below the gas floor.

**It is for monitoring, never for restarting.** Nothing it reports is fixed by
starting the process again, and a restart mid-sync makes the lag worse.

### The dashboard

`ui_enabled: true` mounts a page at `/ui/` with the live wallet list, the flows
in flight, and the master's balances. It has **no authentication** — it belongs
on a laptop over an SSH tunnel, never on an exposed port.

```sh
ssh -L 8800:127.0.0.1:8800 the-box     # then open http://localhost:8800/ui/
```

## Gas: the one thing you must do

The master pays for every transfer: funding a new wallet's first approve, every
drain, every payout. It earns nothing, so it drains monotonically.

```sh
bsc check
```

```
master     0x1A2b…
chain      56
token      0x55d3…  (18 decimals)

native     0.0412  (41200000000000000 wei)
tokens     0  (0 base units)
gas floor  0.05
gas price  1 gwei

BELOW THE GAS FLOOR — nothing refills this automatically.
Run `bsc swap --sell-usdt N` to trade tokens for gas.

router     0x10ED…
price      1 token -> 0.0016 native
           1 native -> 612.4 tokens
```

It exits non-zero below the floor, so it works from cron or a check script. It
reads no database and is safe to run while the service is up.

**Where the tokens come from.** Fees land on the master: a withdrawal asked for
with a `fee` writes a second debit to it (§43). So a service that charges its
users accumulates its own gas budget there without anybody arranging it. A
service that charges nothing has to send tokens or native currency itself.

**Refilling.** Either send native currency to the master directly, or trade the
tokens sitting on it:

```sh
bsc swap --buy-bnb 0.1        # exact output: "I need a tenth of a BNB"
bsc swap --sell-usdt 25       # exact input: "put 25 USDT to work"
bsc swap --buy-bnb 0.1 --dry-run   # quote only
```

Both print the quote and the slippage bound and ask before signing. `--buy-bnb`
is usually what you want: gas is the requirement, and the cost is the answer.
`bsc check` prints the exact command, with the shortfall filled in.

**If you want it automated, automate it in cron, not in the service:**

```cron
# hourly; does nothing unless the master is under gas.floor_wei
17 * * * * /usr/local/bin/bsc swap --buy-bnb 0.1 --if-below --yes >> /var/log/bsc-swap.log 2>&1
```

`--if-below` exits after one balance read when the master is fine, so this is
cheap to run often. The service still never trades on its own — see §44 for why
that line is drawn here rather than inside the daemon, and what an automatic,
predictably-timed, predictably-sized swap looks like to somebody watching the
mempool.

**How much warning the floor gives.** It is a warning line, not empty. At 0.05
native and ~65k gas per transfer, the master has roughly 770 transfers of runway
below it at 1 gwei, and still ~150 at 5 gwei. Hitting it is a "this week"
problem, which is why it is worth a page but not a 3am one.

> **Run it when the service is quiet.** `bsc swap` signs with the same key the
> service signs with, and nonces come from the chain rather than a counter (§7).
> If a transfer is in flight, both can pick the same nonce and one is rejected.
> That is harmless and loud — re-run it — but avoid it during a busy window.

**Alert on this.** `bsc_master_bnb_wei` below `gas.floor_wei` is the one alert
that must not be slept through, because a dry master stops every pipeline at
once and nothing self-corrects:

```yaml
- alert: BscMasterOutOfGas
  expr: bsc_master_bnb_wei < 50000000000000000   # match gas.floor_wei
  for: 15m
  annotations:
    summary: "bsc master is below the gas floor — run `bsc check` then `bsc swap`"
```

## What to watch

| Metric | Why |
| --- | --- |
| `bsc_master_bnb_wei` | The one that stops everything. Alert on it. |
| `bsc_blocks_behind` | Sustained growth means the RPC endpoint or the rate limit. Withdrawals are refused past `chain.max_lag_blocks`. |
| `bsc_in_flight_age_seconds` | A stuck transaction. Alert on the *age*, not the count — the count is 0 or 1 by design. |
| `bsc_balance_underflows_total` | Any value above zero is a bug in our accounting. |
| `bsc_flows_started_total{kind}` | The shape of the work. A drain rate that will not fall usually means a threshold set too low. |

A withdrawal whose `attempts` climbs into double digits is worth an alert of its
own: it means something retrying will not fix, and its amount stays committed
against the wallet while it spins.

## Wallets and topology

A wallet either forwards (`drain_to` set) or accumulates (unset). Retargeting is
a `PATCH`, and it is checked:

```sh
curl -X PATCH localhost:8800/v1/wallets/acme:cust-1 \
  -d '{"drain_to":"0xNewTreasury…"}'
```

A `drain_to` that would form a cycle is refused with `422`. That check matters
more than it looks: `A → B → A` succeeds on every hop, so the only symptom is
your gas draining at the rate blocks arrive. `bsc inspect` re-checks it offline
in case a record was ever edited by hand.

A wallet with a pending withdrawal cannot start forwarding (`409`) — let the
payout settle first, or the drain and the payout race for the same funds.

## Backups

**Set `snapshot.dir`.** Empty disables backup entirely, which is the one
configuration mistake that is unrecoverable.

```yaml
snapshot:
  dir: /var/backups/bsc
  interval: 1h
  keep: 24
```

Snapshots are consistent copies of the whole file, written from inside a read
transaction. They are **plaintext** — encrypt them at whatever ships them off the
box. The master secret is not in them, but the wallet ids are, and those plus the
secret are every private key.

Ship them off the machine. A backup on the same disk protects against nothing
that actually happens.

## Auditing

bsc has no reconciler process. Correctness is checked at the point of spending
(`balanceOf` immediately before every transfer) and on demand:

```sh
bsc inspect /var/backups/bsc/bsc-20260916T120000.db
bsc inspect --rpc /var/backups/bsc/latest.db     # also compare against the token
```

```
/var/backups/bsc/latest.db
  wallets      148 (140 forwarding)
  flows        1
  deposits     9821
  withdrawals  412
  held         184920000000000000000 base units
  committed    5000000000000000000 base units
  audit        clean
  custody      not checked against the chain (re-run with --rpc)
```

Offline it walks every index in both directions, checks that flows and wallets
agree about who owns whom, checks that no drain chain loops, and checks that no
wallet has promised more than it holds. `--rpc` adds the one comparison an
offline pass cannot make: our record of custody against `balanceOf`.

It works on **snapshots**, not the live file: bbolt allows one writer, and the
running service holds it.

Exit code is 0 for clean, 1 for findings — usable from cron.

## When something is wrong

**Nothing has moved for ten minutes.** Look at `/ui/` or the flow list. A flow in
`funding` with a rising `attempt` is almost always gas: check `bsc check`. A flow
with an empty `retry_after` and no transaction is waiting on the sender — check
the logs for a broadcast error.

**Withdrawals are returning 503.** The watcher is more than
`chain.max_lag_blocks` behind, so balances are not current and accepting a payout
could overdraw a wallet. Reads keep working. Check the RPC endpoint and
`chain.rpc_rate_limit`; the watcher needs ~2.2 req/s just to keep pace at 0.45s
blocks, so a limit that cannot outrun the chain never catches up.

**A payout keeps reverting.** It stays `pending` and is retried with a growing
backoff — that is by design (§28), and the money stays committed while it spins.
`last_error` says why. The usual causes are ours and transient. If `attempts` is
in double digits, something is wrong that retrying will not fix; the remedy today
is editing the record, and a cancel command is deliberately unbuilt (see
`docs/ROADMAP.md`).

**The audit reports a solvency finding.** A wallet has promised more than it
holds. That should be impossible — the guard runs before anything is accepted —
so treat it as a real accounting bug: stop accepting withdrawals for that wallet
(`PATCH {"paused":true}`), take a snapshot, and work out which record is wrong
before anything else moves.

**The database will not open.** It is chain-bound: the chain id, token and
decimals are recorded on first run and checked on every later one, so pointing a
populated database at a different chain is refused. That is the check working. If
you genuinely need to move, it is a migration, not a config edit.

## Upgrades

The binary is self-contained and the schema evolves by appending fields, so an
upgrade is: stop, replace, start. Take a snapshot first anyway.

A restart is always safe. Signed transactions are journalled before they are
broadcast, so anything in flight is re-sent byte for byte rather than re-signed,
and the work rules re-derive whatever was pending from current state.
