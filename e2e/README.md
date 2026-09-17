# End-to-end tests against a real chain

Everything in `task unit` runs offline against a chain simulator. The simulator
is faithful — it enforces allowances and balances exactly as the token does — but
it is still our own model, and a model agreeing with itself proves nothing about
gas estimation, finality timing, receipt shapes, or whether the token and the
router behave the way we assume.

This suite closes that gap. It is the only test that spends real funds, so it is
build-tagged and **skips unless configured**.

```sh
task e2e
```

## What it proves

The offline tests cannot reach any of this:

- **Gas estimation is right.** Activation funds a fresh wallet with exactly
  enough for its own `approve`. If the estimate is short the approve fails, and
  only a real chain will tell you.
- **Finality timing works.** Every flow waits for a real finalized receipt, so
  the watcher and the sender have to agree about when a transaction has landed.
- **The token behaves as assumed.** `transferFrom` under the master's allowance,
  the `Transfer` log layout the watcher decodes, and 18 decimals end to end.
- **Forwarding converges on its own.** Nothing enqueues a drain: money lands on a
  wallet with a `drain_to` and the work rules notice, on a chain where blocks
  arrive when they arrive rather than when a test seals one.
- **Amounts survive a real token exactly.** With one unit and no scaling, the
  figure on the deposit is the figure the chain reported, to the last base unit
  — and the payout moves exactly what was asked for, with nothing withheld.
- **A cycle is refused by the running service**, not merely by a unit test.
- **Idempotency on real money.** A retried withdrawal replays instead of paying
  twice, which on a real chain would be unrecoverable.
- **The audit agrees with the chain.** `bsc inspect --rpc` compares every
  materialised custody figure against `balanceOf`, on top of the offline pass
  that walks every index and checks that no wallet owes more than it holds. This
  is the one check that can detect our view of a transfer diverging from the
  token's, and a simulator cannot produce that divergence because it *is* our
  view.
- **The swap actually works** (`TestOperatorSwap`). It is the one operation whose
  outcome is a price rather than a yes or no, and the hardest to fake
  convincingly: a fake router can only confirm our own assumptions about
  calldata. It exercises the quote in both directions, the slippage bound, the
  allowance, and the trade — the same sequence `bsc swap` runs.

## Configuration

| Variable | Meaning |
| --- | --- |
| `BSC_MASTER_SECRET` | **Required.** 32-byte hex. Without it every test skips. |
| `E2E_RPC_URL` | Endpoint. Defaults to the public BSC testnet. |
| `E2E_DEPOSIT` | Tokens the lifecycle test deposits, in base units. Default 1 whole token. |
| `E2E_SWAP_AMOUNT` | Tokens `TestOperatorSwap` trades away. Default 1 whole token. |
| `E2E_TIMEOUT` | Per-run budget. Default 15m. |
| `E2E_ROUTER` | Override the router; otherwise resolved from the chain. |
| `E2E_RECOVER` | `0xaddr[,0xaddr…]` — pull stranded tokens back, for `TestRecoverStrandedTokens`. |

The endpoint is the only chain input, exactly as the service treats it: which
chain this is, the token and the router all follow what it reports once dialled.
Running against **mainnet is refused** unless you also set the guard the harness
prints when it sees one.

## What to fund

The master needs:

- **Native currency for gas.** Every activation, drain and payout is a real
  transaction. A tenth of a BNB is plenty for a full run on testnet.
- **Tokens**, but only for the tests that move them — `needs{}` declares it, so a
  run that never deposits does not demand a deposit's worth.

Teardown pulls tokens back out of every wallet the run derived and returns the
funder's balance to the master, so a passing run costs only gas.

**One exception:** `TestOperatorSwap` trades tokens for native currency and does
**not** trade back. Buying back would be exactly the automation §38 removed, at a
price nobody looked at. The test logs what it sold; top the master up by hand.

## Cost

A full lifecycle run on testnet is roughly a dozen transactions — activation for
two wallets, a forward, a payout, and the teardown transfers. On testnet that is
free in every sense that matters. On mainnet, at 1 gwei, it is cents of gas plus
whatever the swap test trades.
