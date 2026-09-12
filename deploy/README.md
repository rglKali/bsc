# Deploying bsc

One binary, one systemd unit, one file on disk. No database server, no message
broker, no Docker.

## 1. Build

```sh
GOOS=linux GOARCH=amd64 task binary   # -> bin/bsc
```

## 2. Lay it out

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin bsc
sudo mkdir -p /opt/bsc/bin /etc/bsc /var/lib/bsc /var/backups/bsc
sudo chown -R bsc:bsc /var/lib/bsc /var/backups/bsc

sudo cp bin/bsc /opt/bsc/bin/
sudo cp deploy/systemd/bsc.service /etc/systemd/system/
sudo systemctl daemon-reload
```

Create `/etc/bsc/bsc.env` with at least `MASTER_SECRET` and `RPC_URL`, then lock
it down — it holds the key to every managed wallet:

```sh
sudo chown root:bsc /etc/bsc/bsc.env && sudo chmod 640 /etc/bsc/bsc.env
```

## 3. Start

```sh
sudo systemctl enable --now bsc
journalctl -u bsc -f
```

The startup log prints the master address. Send it some BNB: it pays gas for
every activation, drain and payout, and nothing tops it up automatically.

## Configuration

Everything comes from the environment. A handful of settings also have flags
(`bsc --help`), which override the environment for a one-off run.

| Variable | Default | Notes |
| --- | --- | --- |
| `MASTER_SECRET` | — | **Required.** 32-byte hex. Signs everything; holds an unlimited allowance on every derived wallet. Secrets manager only. |
| `DB_PATH` | `bsc.db` | The only datastore. Put it under `/var/lib/bsc`. |
| `HTTP_ADDR` | `127.0.0.1:8800` | The only listener. There is no authentication — keep it on loopback. |
| `RPC_URL` | per chain | Defaults to the public endpoint for `CHAIN_ID`. An unknown chain must name one — there is nothing sensible to guess, and pointing a signer at the wrong chain's node is worse than failing. |
| `RPC_RATE_LIMIT` | `20` | Requests per second, shared by everything. **Must clear the block rate with room to spare** — see below. Refused below 5. |
| `CHAIN_ID` | `56` | |
| `TOKEN_ADDRESS` | per chain | Defaults to USDT for `CHAIN_ID`. Defaulting to the mainnet address everywhere would be quietly wrong elsewhere: the contract would not exist and the watcher would simply see nothing. |
| `START_BLOCK` | `0` | Only used on a fresh database. `0` means *the current finalized head*, never genesis. |
| `POLL_INTERVAL` | `500ms` | Head poll once caught up; no sleep at all while behind. |
| `BACKFILL_BATCH` | `100` | Blocks per write transaction while catching up. |
| `MAX_LAG_BLOCKS` | `200` | Withdrawals are refused past this (~90s). Reads keep working. |
| `DRAIN_THRESHOLD_WEI` | `1e18` | Don't spend gas moving less than this, applied to a deposit wallet's total. It has no effect on what an app is credited — anything worth a whole cent is recorded and shows as `pending` until the drain runs. |
| `DEFAULT_FEE_CENTS` | `100` | A newly registered app's fee ($1.00), which it may then change itself. |
| `HOUSE_SWEEP_MIN_CENTS` | `100` | How much a wallet must hold *over* its app's ledger — fees, sub-cent dust, stray transfers — before one transfer is worth collecting it. `0` disables sweeping; the money is still yours and simply accumulates. |
| `FEE_COLLECTOR` | the master | Where the house's money lands. |
| `FUNDING_MULTIPLIER` | `1.25` | Headroom over the estimated activation cost. Keep modest: unused gas is refunded, so the surplus stays as dust in each deposit wallet. |
| `GAS_PRICE_MULTIPLIER` | `1.10` | Bump over the suggested price. These two are the *only* gas knobs; amounts are estimated. |
| `REBROADCAST_AFTER` | `2m` | How long before an unconfirmed transaction is re-sent (the same signed bytes). |
| `MASTER_POLL` | `30s` | How often the master BNB gauge refreshes. |
| `SWAP_ENABLED` | `true` | Trade collected fees back into gas when the master runs low. |
| `SWAP_ROUTER` | per chain | A Uniswap-V2-interface router. Defaults to the verified PancakeSwap V2 deployment for `CHAIN_ID`; an unknown chain must name one or set `SWAP_ENABLED=false`. |
| `SWAP_WRAPPED_NATIVE` | asked of the router | Override only, for a fork that names the accessor something other than `WETH()`. |
| `SWAP_AMOUNT_WEI` | `10e18` | Tokens traded per top-up. |
| `GAS_FLOOR_WEI` | `0.05` BNB | Native balance below which a top-up is due. Well above the cost of the swap itself — below that, the master could not afford to rescue itself. |
| `SWAP_SLIPPAGE_BPS` | `100` (1%) | Bound computed from the router's own quote. |
| `SWAP_COOLDOWN` | `1h` | Minimum gap between attempts. This is what stops a swap that does not lift the balance from trading away every fee. |
| `SNAPSHOT_DIR` | — | Unset disables self-backup. Set it. |
| `SNAPSHOT_INTERVAL` | `1h` | |
| `SNAPSHOT_KEEP` | `24` | Older snapshots are pruned. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. Logs are JSON. |

### On `RPC_RATE_LIMIT`

At ~0.45s block times the watcher needs about 2.2 requests per second just to
keep pace, and it must *outrun* the chain to recover from any downtime. At 20/s
it processes roughly nine times real time, so an hour of downtime clears in about
seven minutes. At 5/s it would barely manage twice real time and a day's outage
would take half a day to unwind.

This is the one setting that can quietly leave the service unable to catch up, so
the config refuses anything below 5 outright.

## Network

Nothing here is public. bsc listens on loopback, has no authentication, and
expects every caller to be a first-party service on the same box — the app slug
in the path is identity, not a credential. Do not put it behind a public
reverse proxy.

`/metrics` is on the same listener and scraped over loopback; see
`prometheus.yml`.

## Backups

With `SNAPSHOT_DIR` set the service writes a consistent copy of the whole
database on a timer, and prunes to `SNAPSHOT_KEEP`. **Ship those off the
machine.** Losing the file loses the wallet UUIDs, and without them every address
ever derived is unrecoverable even though you still hold the master secret.

Snapshots are plaintext — the database holds no secret that is not already on the
box — so encrypt them at whatever ships them, not here.

Audit one at any time; it exits non-zero on findings:

```sh
bsc inspect /var/backups/bsc/bsc-20260911T120000Z.db
bsc inspect --rpc /var/backups/bsc/bsc-20260911T120000Z.db   # also checks balances
```

## Monitoring

`/metrics` is Prometheus-formatted and served from the same loopback listener, so
it is scraped over loopback like everything else here.

| File | For |
| --- | --- |
| `prometheus.yml` | A scrape **fragment** — `scrape_configs` only, no `global` block. The repo-root Prometheus includes it via `scrape_config_files`, so bsc owns its own scrape config. |
| `grafana/prometheus.yml` | A **standalone** config, for pointing a local Prometheus at a local bsc while working on the dashboard. |
| `grafana/bsc-dashboard.json` | Import into Grafana. See [`grafana/README.md`](grafana/README.md) for what each panel means and which two are worth reading carefully rather than at a glance. |

What to alert on is in [`../docs/OPERATING.md`](../docs/OPERATING.md); the short
version is a master that stays low on gas, a watcher that stays behind, and any
solvency shortfall at all.

## Upgrading

```sh
GOOS=linux GOARCH=amd64 task binary
sudo cp bin/bsc /opt/bsc/bin/ && sudo systemctl restart bsc
```

Safe at any moment: every signed transaction is journalled before it is
broadcast, so a restart re-sends the same bytes rather than signing again, and
the work rules re-converge at startup.
