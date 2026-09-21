# Roadmap — possible extensions

**The service is built and none of this is required to run it.** Roughly ordered
by value, nothing committed.

## The service above this one

- [ ] **A ledger service.** This is the big one, and it is the reason §33 removed
  the ledger from bsc rather than improving it. Apps, users, referral trees,
  balances, fees and statements all belong to something that owns those concepts.
  bsc's job in that world is unchanged: derive wallets, report deposits, execute
  withdrawals. Keep the boundary at "who owes whom" — the moment bsc needs to
  answer that, it is a payments provider again.

## Observability

- [ ] **Alerting on the master.** `bsc_master_bnb_wei` below `gas.floor_wei` is
  now an unambiguous page: nothing refills it automatically (§38, §44). A
  `for: 15m` guard is enough, since the balance only ever falls.
- [ ] **Stuck-transaction alert.** Alert on `bsc_in_flight_age_seconds` rather
  than on `bsc_transactions_in_flight`, which is 0 or 1 by design.
- [ ] Tracing across the deposit → drain → settlement path.

## Withdrawals

- [ ] **Cancelling a withdrawal.** A withdrawal has no failure state (§28): a
  payout that reverts is retried forever, so one to a destination that can never
  receive holds its commitment indefinitely and only an operator editing the
  record frees it. A cancel is the obvious remedy and is unbuilt on purpose —
  releasing a commitment for a payout that might still land is the one way this
  design could pay twice. It needs a signed-and-broadcast check that is certain,
  not merely probable, which in practice means "no journal entry and no in-flight
  tx for this withdrawal" evaluated inside the same write transaction.

## Topology

- [ ] **Sweeping a whole subtree on demand.** `drain_to` forwards on a threshold;
  there is no way to say "move everything now, whatever it is worth". Useful
  before a migration or a wind-down. It is a flow like any other, but it needs a
  deliberate way to bypass `money.drain_threshold_wei` without making the
  threshold meaningless.
- [ ] **Per-wallet drain thresholds.** One global number is blunt when wallets
  differ by orders of magnitude. The field would sit beside `drain_to` and
  default to the config value; the rule already reads a threshold per wallet, so
  this is a record change and nothing else.

## Throughput

- [ ] **Beyond one transaction at a time.** Signing is sequential, capping
  throughput at roughly one transaction per finality window — a second or two at
  current block times. If that ever binds, the path is a persisted nonce counter
  plus concurrent in-flight transactions plus nonce-ordered gap recovery, all
  confined to `sender/`, since every flow is already a state machine that does
  not care how many others are running. The counter would have to resync whenever
  the operator signs from the master by hand.

## Delivery

- [ ] **Streamed pull (SSE).** Real-time without the delivery state that webhooks
  would impose: the caller dials us, and reconnecting carries its cursor, so
  at-least-once and replay still fall out of the deposit log. It is a small
  addition on the same substrate, worth doing the day a poll interval is genuinely
  too slow. See `ARCHITECTURE.md` decision 16.

## Features

- [ ] **Multi-token support.** USDT only today. The token is configurable but
  singular; per-token balances would touch the wallet record and every amount in
  the API. Cheaper than it was — there is no ledger to denominate any more.
- [ ] **Deposit-address expiry or rotation.** Addresses are permanent per ref
  (§17). A caller with a very large, mostly-dormant address book pays for that in
  the watcher's match set — measured, that is ~5 MB of heap and ~40 ms of startup
  at 100k addresses, and the per-block cost does not move at all, since matching
  is local and the rules only visit wallets a block touched. Nothing has needed
  it yet, and at these numbers nothing will for a long while.
- [ ] Per-wallet withdrawal rate limits.

## Housekeeping

- [ ] **A committed-total counter.** `available` sums a wallet's open withdrawals
  on each read. That is proportional to what is in flight rather than to history,
  so it is well bounded already; a counter would fix it outright at the cost of
  more state for the audit to verify — which is exactly the trade §39 declined.
- [ ] Off-box snapshot shipping, with encryption at that boundary.

## Explicitly not doing

- **A ledger, a fee, or a house sweep.** All three existed and all three were
  removed (§33). If one comes back, it belongs in the service above this one.
- **Trading, in any form.** It went out in three steps: the automatic top-up
  (§38), then the attended `bsc swap` command (§53). It is the only operation
  whose outcome is a price rather than a yes or no, and the master is the
  operator's own wallet — so it belongs in whatever they already use to trade,
  not inside a service that holds everybody's keys.
- **Draining the master.** You hold `BSC_MASTER_SECRET` and can move funds out at
  any time; building it in adds risk for no gain.
- **An admin API.** With ref addressing the caller API is already the operator's
  read surface, and no write-side operator action survived scrutiny that is not
  already a CLI command.
- **Webhooks or a broker.** See `ARCHITECTURE.md` decision 16.
