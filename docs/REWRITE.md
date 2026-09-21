# This doc is my own roadmap on this project

# Flows:
1. Only a single "flow" can run per wallet, the others should be queued
2. All described flows are sequential, and should lock the wallet, they are executed on

Activation: Funding -> Approval

1. Withdrawal & Drain flows:
    Unlock wallet -> execute transferFrom

so, overall, "expected" transactions are:
1. funding tx on a wallet -> should trigger the approve transaction
2. approve transaction -> should trigger drain / pending withdrawals
3. drain/withdrawal tx -> write the log

Other events
3. deposit on a wallet -> should trigger or queue the drain, if enabled
4. withdrawal request -> should execute transferFrom, if enabled

Execution of all master transactions MUST be sequential: the lock is acquired once a transaction has been accepted by the chain.

Acquire lock -> request nonce -> publish tx -> release lock

Three types of optimizations to research:
1. manage nonce offchain locally
2. manage bnb balance offchain locally
3. manage usdt balance offchain locally

For the v1-manual we can freely resolve them from chain, but later we should not do this.

1. During the syncing lags the withdrawals, and basically all the flows are temporary paused, so that the process might focus on syncing, spending all allowed RateLimit there. We call it "backfill lock". On startup the watcher acquires the backfill, and it might release, it, only between "ticks". Every tick the watcher fetches the FinalizedBlockNumber, than fetches the BlockReceipts one by one, capped via the RateLimitter. When the diff between new FinalizedBlockNumber and the latest known is 0, the lock is released, and not acquired till the end of the syncing. Even better idea is to make it not as a global syncing lock, but more as a 

As far as our transactions are executed with a delay, we should also implement some local implementation of a "frozen" or "locked" balances to prevent double-spend problems.
