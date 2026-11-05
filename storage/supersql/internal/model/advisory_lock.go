package model

import "context"

type AdvisoryKeySpace uint8

const (
	AdvisoryBigIntKey AdvisoryKeySpace = iota + 1
	AdvisoryIntPairKey
)

type AdvisoryKey struct {
	Space AdvisoryKeySpace
	Value uint64
}

type AdvisoryLockMode uint8

const (
	AdvisoryShared AdvisoryLockMode = iota + 1
	AdvisoryExclusive
)

type AdvisoryLockScope uint8

const (
	AdvisorySessionScope AdvisoryLockScope = iota + 1
	AdvisoryTransactionScope
)

type AdvisoryLockRequest struct {
	Key   AdvisoryKey
	Mode  AdvisoryLockMode
	Scope AdvisoryLockScope
	Wait  bool
}

// AdvisoryLockRuntime is borrowed by one expression invocation. Transaction
// scope is the invocation's current transaction; session scope outlives it.
type AdvisoryLockRuntime interface {
	AcquireAdvisory(context.Context, AdvisoryLockRequest) (bool, error)
	UnlockAdvisory(context.Context, AdvisoryKey, AdvisoryLockMode) (bool, error)
	UnlockAllAdvisory(context.Context) error
}
