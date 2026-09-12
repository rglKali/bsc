# Grafana

`bsc-dashboard.json` — import it and pick your Prometheus datasource.

All series are under one `bsc_*` namespace from a single `/metrics` endpoint;
`prometheus.yml` here is a standalone config for running the dashboard against a
local instance. For the monorepo deployment use `../prometheus.yml`, which is a
fragment included by the repo-root config.

The top row is what an operator actually watches, and
[`../../docs/OPERATING.md`](../../docs/OPERATING.md) explains why each one
matters:

- **Master wallet** — the one to alert on. A dry master stops every pipeline, and
  gas top-ups defend it, but only while there are fees to sell — which is why
  the same panel plots `bsc_master_usdt_wei` beside it. Native falling while
  tokens sit at zero is the combination that needs a human.
- **Blocks behind** — sustained growth means the RPC cannot keep up, and
  withdrawals start being refused once it passes `MAX_LAG_BLOCKS`.
- **In-flight age** — signing is sequential, so this growing means one
  transaction is wedged and everything is queued behind it.

Two panels are worth reading carefully rather than at a glance:

- *Deposits* shows recorded and ignored side by side. Ignored transfers were
  worth less than a whole cent, so there was no ledger entry to write: the two
  series differing is correct, not a fault.
- *Problems* should sit flat at zero. **A solvency shortfall is the serious one**
  — a hot wallet holding less than its app is owed, which nothing self-corrects.
  A balance underflow is a bug in our own accounting; insufficient-balance
  refusals mean our record of custody drifted from the chain.
