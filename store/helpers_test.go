package store

import "sync/atomic"

// nextID hands out distinct wallet ids for fixtures. Tests only need them to be
// unique and non-zero — zero is the master's reserved slot — so a counter is
// exactly the right shape, and it is the shape the store itself uses.
var idSeq atomic.Uint64

func nextID() WalletID { return WalletID(idSeq.Add(1)) }
