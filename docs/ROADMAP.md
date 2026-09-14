# Roadmap — possible extensions

**Status: built.** Nothing here is required; the service runs without any of it.
Roughly ordered by value, none committed.

## Observability

- [ ] **Alerting on a master the top-up cannot save.** Gas top-ups already defend
  `bsc_master_bnb_wei`, so a bare low-balance rule would mostly fire on a
  condition that fixes itself. What is worth alerting on is the residue: the
  balance low *and* staying low, which means there were no fees to sell, the swap
  kept reverting, or `SWAP_ENABLED=false`. Belongs in Grafana, not the service.
- [ ] **Stuck-transaction alert.** Alert on `bsc_in_flight_age_seconds` rather
  than on `bsc_transactions_in_flight`, which is 0 or 1 by design.
- [ ] Tracing across the deposit → drain → settlement path.

## Withdrawals

- [ ] **Cancelling a withdrawal.** A withdrawal has no failure state (§28): a
  payout that reverts is retried forever, so one to a destination that can never
  receive holds its reservation indefinitely and only an operator editing the
  record frees it. A cancel is the obvious remedy and is unbuilt on purpose —
  releasing a reservation for a payout that might still land is the one way this
  design could pay twice. It needs a signed-and-broadcast check that is certain,
  not merely probable, which in practice means "no journal entry and no in-flight
  tx for this withdrawal" evaluated inside the same write transaction that
  releases.

## Throughput

- [ ] **Beyond one transaction at a time.** Signing is sequential, capping
  throughput at roughly one transaction per finality window — a second or two at
  current block times. If that ever binds, the path is a persisted nonce counter
  plus concurrent in-flight transactions plus nonce-ordered gap recovery, all
  confined to `sender/`, since every flow is already a state machine that does
  not care how many others are running. The counter would have to resync
  whenever the operator signs by hand.

## Delivery

- [ ] **Streamed pull (SSE).** Real-time without the delivery state that
  webhooks would impose: the app dials us, and reconnecting carries its cursor,
  so at-least-once and replay still fall out of the deposit log. It is a small
  addition on the same substrate, worth doing the day a poll interval is
  genuinely too slow. See `ARCHITECTURE.md` decision 16.

## Features

- [ ] **Percentage fees.** `FeePolicy` already carries `bps` and `max_fee_cents`,
  refused while non-zero. Implementing them is a change to `FeePolicy.Quote` and
  nothing else — no migration, because both fields already exist on disk. Keep
  the result a whole number of cents: a fractional fee would put a fraction into
  the ledger and cost it the exactness everything else depends on.

- [ ] **Give `FeePolicy` its own version byte.** It is encoded *inside* `App` and
  `Withdrawal` rather than as a record of its own, so the usual "append a field,
  bump the constant" does not work for it: appending shifts every byte after it
  in the parent. Percentage fees do not need this, but any *new* fee field does,
  and it is a few lines before first deploy against a migration afterwards.

- [ ] **Decide what the house dust is ultimately for** (see §25). It accrues and
  is swept as income today, which is standard and is stated in the integrator
  contract. Because the excess is *derived* (`custody − ledger`) rather than
  stored, redirecting some of it later needs no migration — which is exactly why
  the question can stay open.
- [ ] **Multi-token support.** USDT only today. The token is configurable but
  singular; per-token balances would touch the wallet record and every amount in
  the API.
- [ ] Per-app withdrawal rate limits, if `min_cents` proves too blunt a brake.

## Housekeeping

- [ ] **Pending-balance counter.** `…/balance` sums an app's open deposit records
  on each read. That is proportional to what is awaiting a drain rather than to
  how many addresses exist, so it is already much better bounded than it was; a
  counter would fix it outright at the cost of more state for the audit to verify.

- [ ] **Credit a deposit before its drain lands.** Today a deposit is spendable
  only once swept into the hot wallet, because a payout is drawn from there. An
  app could be credited at detection if the withdrawal path were willing to pull
  funds forward — demand-draining the addresses it needs before paying. Better
  for apps, strictly more machinery, and not needed while drains are fast.
- [ ] Off-box snapshot shipping, with encryption at that boundary.

## Explicitly not doing

- **Draining the master.** You hold `MASTER_SECRET` and can move collected fees
  out at any time; building it in adds risk for no gain. (Swapping USDT→BNB was
  in this list and is now **built** — see decisions 19 and 20.)
- **An admin API or UI.** With slug addressing the app API is already the
  operator's read surface, and no write-side operator action survived scrutiny.
- **Webhooks or a broker.** See `ARCHITECTURE.md` decision 16.
