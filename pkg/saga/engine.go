package saga

import (
	"context"
	"sort"
	"time"
)

// NewTransaction creates a transaction in the Pending phase.
func NewTransaction(holder string, items []ItemState, config Config) State {
	cp := make([]ItemState, len(items))
	copy(cp, items)
	return State{
		Phase:  PhasePending,
		Holder: holder,
		Items:  cp,
		Config: config,
	}
}

// Capture records a before-image for the given item. Items not captured
// before Step() are auto-captured during the Prepare phase.
func Capture(ctx context.Context, state State, id ItemID, deps Dependencies) (State, error) {
	s := cloneState(state)
	if err := validate(s); err != nil {
		return state, err
	}

	idx := findItem(s.Items, id)
	if idx < 0 {
		return state, ErrItemNotFound
	}
	if s.Items[idx].Captured {
		return s, nil // idempotent
	}

	if err := deps.Executor.Capture(ctx, s.Items[idx]); err != nil {
		return state, err
	}
	s.Items[idx].Captured = true
	return s, nil
}

// Step advances the transaction by one item. Stateless: takes full state,
// returns new state. The caller drives the loop.
func Step(ctx context.Context, state State, deps Dependencies) (State, Result, error) {
	s := cloneState(state)
	if err := validate(s); err != nil {
		return state, Result{}, err
	}

	if isTerminal(s.Phase) {
		return s, Result{Terminal: true}, nil
	}

	// Timeout check for in-progress phases.
	if s.StartedAt != nil && s.Config.Timeout > 0 {
		deadline := s.StartedAt.Add(s.Config.Timeout)
		if time.Now().After(deadline) {
			return handleTimeout(ctx, s, deps)
		}
	}

	switch s.Phase {
	case PhasePending:
		return stepPending(s)
	case PhasePreparing:
		return stepPreparing(ctx, s, deps)
	case PhasePrepared:
		return stepPrepared(s)
	case PhaseCommitting:
		return stepCommitting(ctx, s, deps)
	case PhaseRollingBack:
		return stepRollingBack(ctx, s, deps)
	default:
		return state, Result{}, &ErrInvalidState{Reason: "unknown phase: " + string(s.Phase)}
	}
}

// --- phase handlers ---

func stepPending(s State) (State, Result, error) {
	now := time.Now()
	s.StartedAt = &now
	s.Phase = PhasePreparing
	return s, Result{}, nil
}

func stepPreparing(ctx context.Context, s State, deps Dependencies) (State, Result, error) {
	for _, i := range sortedIndices(s.Items) {
		item := &s.Items[i]
		if item.Prepared {
			continue
		}

		// Acquire lock.
		lockRef, err := deps.LockManager.Acquire(ctx, item.LockKey, s.Holder, s.Config.LockTimeout)
		if err != nil {
			releaseAll(ctx, s, deps)
			return fail(s, "lock acquisition failed for "+item.ID+": "+err.Error())
		}
		item.LockRef = lockRef

		// Late capture for uncaptured items.
		if !item.Captured {
			if err := deps.Executor.Capture(ctx, *item); err != nil {
				releaseAll(ctx, s, deps)
				return fail(s, item.ID+": capture failed: "+err.Error())
			}
			item.Captured = true
		}

		// Prepare.
		if err := deps.Executor.Prepare(ctx, *item); err != nil {
			releaseAll(ctx, s, deps)
			return fail(s, item.ID+": prepare failed: "+err.Error())
		}
		item.Prepared = true

		return s, Result{}, nil // one item per Step
	}

	// All prepared — transition.
	s.Phase = PhasePrepared
	return s, Result{}, nil
}

func stepPrepared(s State) (State, Result, error) {
	s.Phase = PhaseCommitting
	return s, Result{}, nil
}

func stepCommitting(ctx context.Context, s State, deps Dependencies) (State, Result, error) {
	for _, i := range sortedIndices(s.Items) {
		item := &s.Items[i]
		if item.Committed {
			continue
		}

		// Renew lock.
		if err := deps.LockManager.Renew(ctx, item.LockRef, s.Holder, s.Config.LockTimeout); err != nil {
			return triggerRollback(s, "lock renewal failed for "+item.ID+": "+err.Error())
		}

		// Pre-commit check. Any error is fatal — no rollback.
		if err := deps.Executor.PreCommitCheck(ctx, *item); err != nil {
			return fail(s, item.ID+": pre-commit check failed: "+err.Error())
		}

		// Forward.
		if err := deps.Executor.Forward(ctx, *item); err != nil {
			if IsSkippable(err) {
				return fail(s, item.ID+": "+err.Error())
			}
			item.Error = err.Error()
			return triggerRollback(s, item.ID+": forward failed: "+err.Error())
		}
		item.Committed = true

		return s, Result{}, nil // one item per Step
	}

	// All committed — release locks, forget, terminal.
	releaseAll(ctx, s, deps)
	forgetAll(ctx, s, deps)
	now := time.Now()
	s.CompletedAt = &now
	s.Phase = PhaseCommitted
	return s, Result{Terminal: true}, nil
}

func stepRollingBack(ctx context.Context, s State, deps Dependencies) (State, Result, error) {
	for _, i := range reverseSortedIndices(s.Items) {
		item := &s.Items[i]
		if !item.Committed || item.RolledBack || item.Skipped {
			continue
		}

		if err := deps.Executor.Reverse(ctx, *item); err != nil {
			if IsSkippable(err) {
				item.Skipped = true
				item.Error = err.Error()
				return s, Result{}, nil
			}
			// Retryable error.
			item.Error = err.Error()
			item.RetryCount++
			if deps.RetrySignal != nil && deps.RetrySignal.ShouldRetry() {
				return s, Result{RequeueAfter: backoff(item.RetryCount)}, nil
			}
			// No retry — fail.
			releaseAll(ctx, s, deps)
			return fail(s, item.ID+": reverse failed: "+err.Error())
		}

		item.RolledBack = true
		item.Error = ""
		deps.Executor.Forget(ctx, *item) // best-effort, error ignored
		return s, Result{}, nil          // one item per Step
	}

	// All items processed.
	releaseAll(ctx, s, deps)
	now := time.Now()
	s.CompletedAt = &now

	if hasUnrolledCommits(s) {
		s.Phase = PhaseFailed
		s.Error = "rollback completed with unresolved items"
		return s, Result{Terminal: true}, nil
	}

	s.Phase = PhaseRolledBack
	return s, Result{Terminal: true}, nil
}

// --- timeout ---

func handleTimeout(ctx context.Context, s State, deps Dependencies) (State, Result, error) {
	if s.Phase == PhaseRollingBack {
		releaseAll(ctx, s, deps)
		return fail(s, "rollback timed out")
	}
	if hasUnrolledCommits(s) {
		now := time.Now()
		s.StartedAt = &now // fresh window for rollback
		return triggerRollback(s, "transaction timed out with committed items")
	}
	releaseAll(ctx, s, deps)
	return fail(s, "transaction timed out")
}

// --- helpers ---

func validate(s State) error {
	if s.Holder == "" {
		return &ErrInvalidState{Reason: "empty holder"}
	}
	if len(s.Items) == 0 {
		return &ErrInvalidState{Reason: "no items"}
	}
	return nil
}

func cloneState(s State) State {
	c := s
	c.Items = make([]ItemState, len(s.Items))
	copy(c.Items, s.Items)
	if s.StartedAt != nil {
		t := *s.StartedAt
		c.StartedAt = &t
	}
	if s.CompletedAt != nil {
		t := *s.CompletedAt
		c.CompletedAt = &t
	}
	return c
}

func fail(s State, reason string) (State, Result, error) {
	now := time.Now()
	s.CompletedAt = &now
	s.Phase = PhaseFailed
	s.Error = reason
	return s, Result{Terminal: true}, nil
}

func triggerRollback(s State, reason string) (State, Result, error) {
	if s.Config.AutoRollback {
		s.Phase = PhaseRollingBack
		return s, Result{}, nil
	}
	return fail(s, reason)
}

func releaseAll(ctx context.Context, s State, deps Dependencies) {
	var refs []string
	for _, item := range s.Items {
		if item.LockRef != "" {
			refs = append(refs, item.LockRef)
		}
	}
	if len(refs) > 0 {
		deps.LockManager.ReleaseAll(ctx, refs, s.Holder) // best-effort
	}
}

func forgetAll(ctx context.Context, s State, deps Dependencies) {
	for _, item := range s.Items {
		deps.Executor.Forget(ctx, item) // best-effort
	}
}

func backoff(retryCount int) time.Duration {
	return min(time.Second<<uint(retryCount), 30*time.Second)
}

func hasUnrolledCommits(s State) bool {
	for _, item := range s.Items {
		if item.Committed && !item.RolledBack {
			return true
		}
	}
	return false
}

func findItem(items []ItemState, id ItemID) int {
	for i := range items {
		if items[i].ID == id {
			return i
		}
	}
	return -1
}

// sortedIndices returns item indices sorted by (Order, ID) ascending.
func sortedIndices(items []ItemState) []int {
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		if items[idx[a]].Order != items[idx[b]].Order {
			return items[idx[a]].Order < items[idx[b]].Order
		}
		return items[idx[a]].ID < items[idx[b]].ID
	})
	return idx
}

// reverseSortedIndices returns item indices sorted by (Order, ID) descending.
func reverseSortedIndices(items []ItemState) []int {
	idx := sortedIndices(items)
	for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
		idx[i], idx[j] = idx[j], idx[i]
	}
	return idx
}
