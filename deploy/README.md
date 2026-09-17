# Deploying bsc

One binary, one systemd unit, one file on disk. No database server, no message
broker, no Docker.

Everything here is flat; each file is copied somewhere on the box:

| File | Goes to |
| --- | --- |
| `bsc.service` | `/etc/systemd/system/` |
| `config.yaml` | copy to `/etc/bsc/config.yaml` — every setting, documented |
| `prometheus.yml` | included by the host's Prometheus |
| `dashboard.json` | imported into Grafana |

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
sudo cp deploy/bsc.service /etc/systemd/system/
sudo systemctl daemon-reload
```

Configuration is two files, and the split is deliberate. `deploy/config.yaml`
holds every operational setting with its default and what it does — copy it to
`/etc/bsc/config.yaml` and edit what you need. The master secret goes somewhere
else entirely, because a file containing it has to be unreadable and
unversionable and the rest of your configuration should not have to be (§30).

```sh
sudo cp deploy/config.yaml /etc/bsc/
```

It needs no special permissions — root-owned and world-readable is correct, and
being able to leave it that way is the point of keeping the secret out of it.

Then the secret. The unit loads it from **`/etc/bsc/bsc.env`**, which holds that
one value and nothing else. There is no example of it in the repo — create it on
the box from wherever you keep secrets:

```sh
printf 'BSC_MASTER_SECRET=%s\n' "$(your-secrets-manager get bsc/master)" \
  | sudo tee /etc/bsc/bsc.env > /dev/null
sudo chown root:bsc /etc/bsc/bsc.env && sudo chmod 640 /etc/bsc/bsc.env
```

Anything in `config.yaml` can also be overridden there as `BSC_*`, which is
useful during an incident. It is not a place to keep configuration: nothing in
it is reviewable.

## 3. Start

```sh
sudo systemctl enable --now bsc
journalctl -u bsc -f
```

The startup log prints the master address. Send it some BNB: it pays gas for
every activation, drain and payout, and nothing tops it up automatically.

## Configuration

Two files. `config.yaml` is every operational setting, each one documented with
its default; `bsc.env` is the master secret and nothing else. The split exists
so that the secret's secrecy does not have to spread to the two dozen other
settings, which are then safe to review, diff and keep in version control (§30).

**`deploy/config.yaml` is the reference.** It lists every setting that exists,
so there is no second table here to drift out of step with it — and
`TestShippedConfigMatchesTheDefaults` parses that file on every run and fails if
any value in it stops matching the binary's default.

Every setting is addressable both ways. An environment variable is the key path
in upper case with `BSC_` in front:

| In the file | In the environment |
| --- | --- |
| `db_path` | `BSC_DB_PATH` |
| `chain.rpc_rate_limit` | `BSC_CHAIN_RPC_RATE_LIMIT` |
| `gas.floor_wei` | `BSC_GAS_FLOOR_WEI` |
| `money.drain_threshold_wei` | `BSC_MONEY_DRAIN_THRESHOLD_WEI` |

Precedence is **flag, then environment, then file, then default** — so you can
override one value during an incident without editing a reviewed file, and the
edit you did not make is not one you have to remember to revert.

A handful of settings also have flags (`bsc --help`): `--db`, `--http-addr`,
`--rpc-url`, `--rpc-rate-limit`, `--snapshot-dir`, `--ui`, `--log-level`, and
`--config` for the file itself.

> **`BSC_MASTER_SECRET` is required, and is environment-only by policy rather
> than by mechanism.** 32-byte hex. It signs everything and holds an unlimited
> allowance on every derived wallet, so whoever has it can move every wallet's
> funds. Secrets manager only — never in `config.yaml`, never in git.

### On `chain.rpc_rate_limit`

At ~0.45s block times the watcher needs about 2.2 requests per second just to
keep pace, and it must *outrun* the chain to recover from any downtime. At 20/s
it processes roughly nine times real time, so an hour of downtime clears in about
seven minutes. At 5/s it would barely manage twice real time and a day's outage
would take half a day to unwind.

This is the one setting that can quietly leave the service unable to catch up, so
the config refuses anything below 5 outright.

## Network

Nothing here is public. bsc listens on loopback, has no authentication, and
expects every caller to be a first-party service on the same box — a ref in the
path is a name rather than a credential, and any caller can read any wallet
(§37). Do not put it behind a public reverse proxy.

`/metrics` is on the same listener and scraped over loopback; see
`prometheus.yml`.

## Backups

With `snapshot.dir` set the service writes a consistent copy of the whole
database on a timer, and prunes to `snapshot.keep`. **Ship those off the
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

`prometheus.yml` is a scrape **fragment** — `scrape_configs` only, no `global`
block. The host's Prometheus includes it via `scrape_config_files`, so bsc owns
its own scrape config.

`dashboard.json` is the Grafana dashboard: import it and pick your Prometheus
datasource. All series are under one `bsc_*` namespace from that single endpoint.

The top row is what an operator actually watches, and
[`../docs/OPERATING.md`](../docs/OPERATING.md) explains why each one matters:

- **Master wallet** — the one to alert on. A dry master stops every pipeline, and
  **nothing refills it**: the automatic top-up was removed along with the fee
  that funded it (§38). The same panel plots `bsc_master_usdt_wei` beside it,
  because tokens parked there are what `bsc swap` can trade for gas. Native
  below `gas.floor_wei` is a page, not a warning.
- **Blocks behind** — sustained growth means the RPC cannot keep up, and
  withdrawals start being refused once it passes `chain.max_lag_blocks`.
- **In-flight age** — signing is sequential, so this growing means one
  transaction is wedged and everything is queued behind it.

Two panels are worth reading carefully rather than at a glance:

- *Deposits* shows transfers seen against deposits recorded. The two differ by
  transfers touching the master, which is bsc's own wallet and not a deposit
  anybody is waiting on. There is no longer an "ignored" series: with the
  chain's own units there is no amount too small to record (§36).
- *Problems* should sit flat at zero. A balance underflow is a bug in our own
  accounting; insufficient-balance refusals mean our record of custody drifted
  from the chain.

What to alert on is in [`../docs/OPERATING.md`](../docs/OPERATING.md); the short
version is a master that stays low on gas, a watcher that stays behind, and any
balance underflow at all.

## Upgrading

```sh
GOOS=linux GOARCH=amd64 task binary
sudo cp bin/bsc /opt/bsc/bin/ && sudo systemctl restart bsc
```

Safe at any moment: every signed transaction is journalled before it is
broadcast, so a restart re-sends the same bytes rather than signing again, and
the work rules re-converge at startup.
