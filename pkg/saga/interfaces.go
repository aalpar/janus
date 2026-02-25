package saga

import (
	"context"
	"time"
)

// StepExecutor performs the actual work for each transaction item.
// Error return types are control flow signals to the engine:
//   - nil: success
//   - *Skippable: item cannot be completed, skip it
//   - other error: transient failure
type StepExecutor interface {
	Capture(ctx context.Context, item ItemState) error
	Prepare(ctx context.Context, item ItemState) error
	PreCommitCheck(ctx context.Context, item ItemState) error
	Forward(ctx context.Context, item ItemState) error
	Reverse(ctx context.Context, item ItemState) error
	Forget(ctx context.Context, item ItemState) error
}

// LockManager provides advisory locking per resource.
// Keys and lock refs are opaque strings — the backend decides the format.
type LockManager interface {
	Acquire(ctx context.Context, key string, holder string, timeout time.Duration) (lockRef string, err error)
	Release(ctx context.Context, lockRef string, holder string) error
	ReleaseAll(ctx context.Context, lockRefs []string, holder string) error
	Renew(ctx context.Context, lockRef string, holder string, timeout time.Duration) error
}

// RetrySignal controls whether the engine should keep retrying a failed
// rollback step. Polled at the start of each retry attempt.
type RetrySignal interface {
	ShouldRetry() bool
}

// Dependencies bundles the interfaces the engine requires.
type Dependencies struct {
	Executor    StepExecutor
	LockManager LockManager
	RetrySignal RetrySignal
}
