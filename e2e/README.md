# End-to-end tests against a real chain

Everything in `task test` runs offline against a chain simulator. The simulator
is faithful — it enforces allowances and balances exactly as the token does — but
it is still our own model, and a model agreeing with itself proves nothing about
gas estimation, finality timing, receipt shapes, or whether the token behaves the
way we assume.

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
- **The audit agrees with the chain.** `bsc inspect --rpc` compares every
  materialised custody figure against `balanceOf`, on top of the offline pass
  that recomputes each ledger from the log and checks solvency. This is the one
  check that can detect our view of a transfer diverging from the token's, and a
  simulator cannot produce that divergence because it *is* our view.
- **The units survive a real token.** The ledger's scale is read from the
  contract's own `decimals()`, and flooring to cents happens against amounts the
  chain actually reported.
- **Idempotency on real money.** A retried withdrawal replays instead of paying
  twice, which on a real chain would be unrecoverable.
- **The swap actually works.** Trading fees back into gas is the only operation
  whose outcome is a price rather than a yes or no, and the hardest to fake
  convincingly: a simulator can only confirm our own assumptions about a router.

## What each test covers

| Test | Walks through |
| --- | --- |
| `TestLifecycle` | register an app → its hot wallet activates → derive a deposit address → an outside wallet deposits → the deposit wallet is funded and activated → the drain sweeps it and credits the ledger → a withdrawal pays out → the house sweep collects the fee → the audit agrees with the chain |
| `TestGasTopUpSwap` | the master trades collected fees for native gas through a real router |
| `TestRejectsOverdraft` | a payout beyond the balance is refused before anything is signed |

Only the **master wallet is fixed** — you fund it once, and the suite puts back
what it spends. Everything else is fresh
on each run: a generated funder, a random app slug, a newly derived hot wallet, a
newly derived deposit address. Nothing carries over between runs except the one
wallet you control.

## Setup

You need one funded wallet. Everything else defaults from the chain id.

| Variable | Required | What |
| --- | --- | --- |
| `E2E_RPC_URL` | no | **The one chain input**, defaulting to the public Chapel endpoint. Which chain this is, the token and the router all follow what it reports — as does the mainnet refusal below. |
| `E2E_MASTER_SECRET` | yes | 32-byte hex. A **throwaway** key holding the run's funds. Everything else is derived or generated. |
| `E2E_TOKEN_ADDRESS` | no | Defaults to the USDT deployment for the chain the endpoint reports. Set it only to use your own ERC-20. |
| `E2E_DEPOSIT` | no | Deposit size in wei. Default 2 whole tokens. |
| `E2E_SWAP_ROUTER` | no | Defaults to the verified PancakeSwap V2 router for the chain. |
| `E2E_SWAP_AMOUNT` | no | Tokens per gas top-up, in wei. Default 1 whole token. |
| `E2E_DESTINATION` | no | Payout target. **Defaults to the funder**, so the paid-out tokens come back. |
| `E2E_FEE_COLLECTOR` | no | Fee target. **Defaults to the master**, matching production. |
| `E2E_TIMEOUT` | no | Whole-suite budget. Default `15m`. |

## What you need to fund

**One wallet.** You fund the master; the suite does the rest.

On BSC testnet the native currency is **tBNB** — plain native BNB on the Chapel
testnet, free from the faucet, not a token and not wrapped. It plays exactly the
role BNB plays on mainnet: it pays gas. (Not to be confused with **WBNB**, the
ERC-20 that wraps BNB; that only appears inside a swap path, and the service
never holds any.)

| Wallet | Needs | Why |
| --- | --- | --- |
| **master** | **~0.05 tBNB** and **≥ 2 test USDT** | It pays gas for everything, stocks the funder at the start of each run, and collects it all back at the end. |

Two tokens is the largest a single test holds at once (the lifecycle's deposit),
and the balance is restored at the end of every run, so this is a one-time top-up
rather than a running cost.

Each run **generates a fresh funder wallet**, moves it the tokens it needs plus
0.01 tBNB of gas, and empties it back into the master when the run finishes. An
outside depositor should not be a long-lived fixture whose leftovers leak between
runs, and generating it means only one wallet ever needs topping up by hand.

### What a run actually costs

**Gas, and only gas.** Every token movement is a closed loop: the payout returns
to the funder, the fee goes to the master, the funder is emptied at the end, and
the one token the gas top-up genuinely spends is **bought back from the same
router during teardown** — that flow converts tokens into native currency, so
teardown converts them back. The master ends each run holding exactly what it
started with.

What a run actually consumes:

- **gas** — roughly 0.01 tBNB at testnet prices;
- **the swap's round-trip spread** — buying a token back costs slightly more than
  selling it earned, a few ten-thousandths of a tBNB at current pool depth;
- **a few thousandths of a tBNB** left permanently in each freshly derived wallet.
  Activation funds a wallet for its own `approve` and unused gas is refunded, so
  the remainder sits there. Reclaiming it would cost more than it is worth.

So 0.05 tBNB is several runs' worth, and the token balance does not drift at all.
The buy-back keeps back 0.02 tBNB for the next run and skips itself rather than
dip below that: being a token short is an inconvenience, being gas short means
nothing runs.

### Getting the token

On Chapel the suite defaults to `usdt.TestnetAddress`. You need a balance of it
in the **master** wallet — get some from a faucet that dispenses that token, or
swap for it on a testnet DEX. The suite moves what each run needs into the
generated funder and returns it afterwards.

If you would rather control the supply, **deploy your own ERC-20** and set
`E2E_TOKEN_ADDRESS`. It only needs the standard `transfer`, `approve`,
`transferFrom`, `balanceOf`, `allowance` and `decimals`.

Whatever you use must have **18 decimals**. The service itself handles any token
with at least two — it reads `decimals()` and derives the cent from it — but this
suite is written in whole tokens, so it checks at startup and refuses to run
otherwise rather than letting every figure here be wrong by orders of magnitude
while appearing to work.

### Getting gas

Fund the master with native tBNB from the BSC testnet faucet. It pays for every
activation, drain, payout, house sweep and swap, and it stocks the generated
funder with the small amount that wallet needs.

### Running

```sh
export E2E_MASTER_SECRET=…   # throwaway, holds the tBNB and test USDT
task e2e
```

Everything else has a default: the endpoint is Chapel unless you say otherwise,
and the token and swap router follow whichever chain it turns out to be.

Expect it to take several minutes: each stage waits for real finality, and the
full lifecycle is roughly eight confirmations deep — stocking the funder is two,
activation two, the drain one, the payout one, the house sweep one.

The suite prints what it is waiting for every twenty seconds, and dumps the live
flows if it times out, so a stuck run tells you which stage and which
transaction rather than just failing.

## Safety

- It **refuses to run against mainnet** unless you also set
  `E2E_I_MEAN_MAINNET=yes`. The chain is read from the endpoint after connecting,
  so pointing `E2E_RPC_URL` at a mainnet node trips the guard — which the old
  check against a configured chain id would have sailed straight past. The suite
  derives fresh wallets and moves funds, and none of that is reversible.
- It uses a fresh temporary database per run, never your real one.
- The funder is generated per run and emptied back into the master afterwards,
  tokens stranded in derived wallets are pulled back under the master's
  allowance, and any remaining shortfall is bought back from the router — three
  layers that together restore the master's opening balance. All of it is best
  effort and logged, never failed: a teardown problem must not mask the result of
  the test that just ran.
- Each run registers apps under a random `e2e-…` slug, so repeated runs do not
  collide.
- It **pre-flights** everything and fails immediately with a clear message: the
  master's gas and token balances, that both cover what the run needs, and that
  the token really is an ERC-20 with 18 decimals at that address. Far better than
  timing out twelve minutes later on a wallet that was never funded.

## What it does not cover

- Re-broadcast of a dropped transaction: it needs a transaction to actually be
  dropped, which is not something a test can arrange reliably.
- The swap test **skips** rather than fails when the router cannot quote the
  pair — a testnet pool that does not exist is an environment problem, not a
  defect. Point `E2E_SWAP_ROUTER` or `E2E_TOKEN_ADDRESS` at a pair with
  liquidity to exercise it.
- Chain reorganisations: everything here reads finalized blocks only.
- Backfill after a long outage. Simulated offline; reproducing it here would mean
  leaving the service down for hours.
