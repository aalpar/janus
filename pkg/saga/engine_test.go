package saga

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- mock types ---

type mockExecutor struct {
	capture        func(ctx context.Context, item ItemState) error
	prepare        func(ctx context.Context, item ItemState) error
	preCommitCheck func(ctx context.Context, item ItemState) error
	forward        func(ctx context.Context, item ItemState) error
	reverse        func(ctx context.Context, item ItemState) error
	forget         func(ctx context.Context, item ItemState) error
	capturedItems  []ItemID
	preparedItems  []ItemID
	committedItems []ItemID
	reversedItems  []ItemID
	forgottenItems []ItemID
	checkedItems   []ItemID
}

func (m *mockExecutor) Capture(ctx context.Context, item ItemState) error {
	m.capturedItems = append(m.capturedItems, item.ID)
	if m.capture != nil {
		return m.capture(ctx, item)
	}
	return nil
}

func (m *mockExecutor) Prepare(ctx context.Context, item ItemState) error {
	m.preparedItems = append(m.preparedItems, item.ID)
	if m.prepare != nil {
		return m.prepare(ctx, item)
	}
	return nil
}

func (m *mockExecutor) PreCommitCheck(ctx context.Context, item ItemState) error {
	m.checkedItems = append(m.checkedItems, item.ID)
	if m.preCommitCheck != nil {
		return m.preCommitCheck(ctx, item)
	}
	return nil
}

func (m *mockExecutor) Forward(ctx context.Context, item ItemState) error {
	m.committedItems = append(m.committedItems, item.ID)
	if m.forward != nil {
		return m.forward(ctx, item)
	}
	return nil
}

func (m *mockExecutor) Reverse(ctx context.Context, item ItemState) error {
	m.reversedItems = append(m.reversedItems, item.ID)
	if m.reverse != nil {
		return m.reverse(ctx, item)
	}
	return nil
}

func (m *mockExecutor) Forget(ctx context.Context, item ItemState) error {
	m.forgottenItems = append(m.forgottenItems, item.ID)
	if m.forget != nil {
		return m.forget(ctx, item)
	}
	return nil
}

type mockLockManager struct {
	acquire    func(ctx context.Context, key, holder string, timeout time.Duration) (string, error)
	release    func(ctx context.Context, lockRef, holder string) error
	releaseAll func(ctx context.Context, lockRefs []string, holder string) error
	renew      func(ctx context.Context, lockRef, holder string, timeout time.Duration) error
	acquired   []string // keys
	renewed    []string // lockRefs
	released   bool
}

func (m *mockLockManager) Acquire(ctx context.Context, key, holder string, timeout time.Duration) (string, error) {
	m.acquired = append(m.acquired, key)
	if m.acquire != nil {
		return m.acquire(ctx, key, holder, timeout)
	}
	return "lock-" + key, nil
}

func (m *mockLockManager) Release(ctx context.Context, lockRef, holder string) error {
	if m.release != nil {
		return m.release(ctx, lockRef, holder)
	}
	return nil
}

func (m *mockLockManager) ReleaseAll(ctx context.Context, lockRefs []string, holder string) error {
	m.released = true
	if m.releaseAll != nil {
		return m.releaseAll(ctx, lockRefs, holder)
	}
	return nil
}

func (m *mockLockManager) Renew(ctx context.Context, lockRef, holder string, timeout time.Duration) error {
	m.renewed = append(m.renewed, lockRef)
	if m.renew != nil {
		return m.renew(ctx, lockRef, holder, timeout)
	}
	return nil
}

type mockRetrySignal struct {
	shouldRetry bool
}

func (m *mockRetrySignal) ShouldRetry() bool { return m.shouldRetry }

// --- test helpers ---

func twoItems() []ItemState {
	return []ItemState{
		{ID: "a", Order: 1, LockKey: "key-a"},
		{ID: "b", Order: 2, LockKey: "key-b"},
	}
}

func defaultConfig() Config {
	return Config{
		Timeout:      5 * time.Minute,
		LockTimeout:  30 * time.Second,
		AutoRollback: true,
	}
}

func defaultDeps(exec *mockExecutor, locks *mockLockManager) Dependencies {
	return Dependencies{
		Executor:    exec,
		LockManager: locks,
	}
}

// stepN calls Step n times, failing the test on any error.
func stepN(t *testing.T, ctx context.Context, s State, deps Dependencies, n int) State {
	t.Helper()
	for i := range n {
		var result Result
		var err error
		s, result, err = Step(ctx, s, deps)
		if err != nil {
			t.Fatalf("Step %d: unexpected error: %v", i, err)
		}
		if result.Terminal {
			return s
		}
	}
	return s
}

// --- tests ---

func TestNewTransaction(t *testing.T) {
	items := twoItems()
	s := NewTransaction("holder-1", items, defaultConfig())

	if s.Phase != PhasePending {
		t.Errorf("Phase = %q, want Pending", s.Phase)
	}
	if s.Holder != "holder-1" {
		t.Errorf("Holder = %q, want holder-1", s.Holder)
	}
	if len(s.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(s.Items))
	}
	// Verify it's a copy, not the same slice.
	items[0].ID = "mutated"
	if s.Items[0].ID == "mutated" {
		t.Error("NewTransaction did not copy items slice")
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{
		Executor:    &mockExecutor{},
		LockManager: &mockLockManager{},
	}

	tests := []struct {
		name   string
		state  State
		wantRe string
	}{
		{
			name:   "empty holder",
			state:  State{Phase: PhasePending, Items: twoItems()},
			wantRe: "empty holder",
		},
		{
			name:   "no items",
			state:  State{Phase: PhasePending, Holder: "h", Items: nil},
			wantRe: "no items",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Step(ctx, tt.state, deps)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var inv *ErrInvalidState
			if !errors.As(err, &inv) {
				t.Fatalf("expected ErrInvalidState, got %T: %v", err, err)
			}
			if inv.Reason != tt.wantRe {
				t.Errorf("Reason = %q, want %q", inv.Reason, tt.wantRe)
			}
		})
	}
}

func TestValidationCapture(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Executor: &mockExecutor{}, LockManager: &mockLockManager{}}

	_, err := Capture(ctx, State{Phase: PhasePending, Items: twoItems()}, "a", deps)
	if err == nil {
		t.Fatal("expected error for empty holder")
	}
}

func TestFullHappyPath(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())

	// Step 1: Pending → Preparing
	s = stepN(t, ctx, s, deps, 1)
	if s.Phase != PhasePreparing {
		t.Fatalf("Phase = %q, want Preparing", s.Phase)
	}
	if s.StartedAt == nil {
		t.Fatal("StartedAt not set")
	}

	// Steps 2-3: prepare items a, b (one per step)
	s = stepN(t, ctx, s, deps, 2)
	if s.Phase != PhasePreparing {
		t.Fatalf("Phase = %q, want Preparing (still processing)", s.Phase)
	}

	// Step 4: no unprepared items → Prepared
	s = stepN(t, ctx, s, deps, 1)
	if s.Phase != PhasePrepared {
		t.Fatalf("Phase = %q, want Prepared", s.Phase)
	}

	// Step 5: Prepared → Committing
	s = stepN(t, ctx, s, deps, 1)
	if s.Phase != PhaseCommitting {
		t.Fatalf("Phase = %q, want Committing", s.Phase)
	}

	// Steps 6-7: commit items a, b (one per step)
	s = stepN(t, ctx, s, deps, 2)
	if s.Phase != PhaseCommitting {
		t.Fatalf("Phase = %q, want Committing (still processing)", s.Phase)
	}

	// Step 8: no uncommitted items → Committed (terminal)
	var result Result
	var err error
	s, result, err = Step(ctx, s, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Phase != PhaseCommitted {
		t.Fatalf("Phase = %q, want Committed", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal=true")
	}
	if s.CompletedAt == nil {
		t.Error("CompletedAt not set")
	}

	// Verify ordering.
	if len(exec.capturedItems) != 2 || exec.capturedItems[0] != "a" || exec.capturedItems[1] != "b" {
		t.Errorf("captured = %v, want [a b]", exec.capturedItems)
	}
	if len(exec.preparedItems) != 2 || exec.preparedItems[0] != "a" || exec.preparedItems[1] != "b" {
		t.Errorf("prepared = %v, want [a b]", exec.preparedItems)
	}
	if len(exec.committedItems) != 2 || exec.committedItems[0] != "a" || exec.committedItems[1] != "b" {
		t.Errorf("committed = %v, want [a b]", exec.committedItems)
	}
	// Forget called for all items on terminal commit.
	if len(exec.forgottenItems) != 2 {
		t.Errorf("forgotten = %v, want 2 items", exec.forgottenItems)
	}
	if !locks.released {
		t.Error("locks not released on commit")
	}
}

func TestOneItemPerStep(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())

	// Pending → Preparing
	s = stepN(t, ctx, s, deps, 1)

	// First Step in Preparing should prepare exactly one item.
	s = stepN(t, ctx, s, deps, 1)
	if len(exec.preparedItems) != 1 {
		t.Errorf("prepared after 1 step = %d, want 1", len(exec.preparedItems))
	}

	// Second step prepares item b.
	s = stepN(t, ctx, s, deps, 1)
	if len(exec.preparedItems) != 2 {
		t.Errorf("prepared after 2 steps = %d, want 2", len(exec.preparedItems))
	}
	// Phase still Preparing until the transition step.
	if s.Phase != PhasePreparing {
		t.Errorf("Phase = %q, want Preparing", s.Phase)
	}

	// Third step: no more unprepared → Prepared.
	s = stepN(t, ctx, s, deps, 1)
	if s.Phase != PhasePrepared {
		t.Errorf("Phase = %q, want Prepared", s.Phase)
	}
}

func TestOrderingSortByOrderThenID(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	items := []ItemState{
		{ID: "z", Order: 1, LockKey: "k-z"},
		{ID: "a", Order: 2, LockKey: "k-a"},
		{ID: "m", Order: 1, LockKey: "k-m"},
	}
	s := NewTransaction("tx-1", items, defaultConfig())

	// Drive through prepare: Pending→Preparing, then 3 prepare steps, then Prepared.
	s = stepN(t, ctx, s, deps, 5)
	if s.Phase != PhasePrepared {
		t.Fatalf("Phase = %q, want Prepared", s.Phase)
	}

	// Order 1: m, z (by ID); Order 2: a.
	want := []string{"m", "z", "a"}
	if len(exec.preparedItems) != 3 {
		t.Fatalf("prepared = %v, want %v", exec.preparedItems, want)
	}
	for i, id := range want {
		if exec.preparedItems[i] != id {
			t.Errorf("prepared[%d] = %q, want %q", i, exec.preparedItems[i], id)
		}
	}
}

func TestCaptureEarly(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())

	// Early capture of item a.
	var err error
	s, err = Capture(ctx, s, "a", deps)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !s.Items[0].Captured {
		t.Error("item a not marked captured")
	}

	// Drive to Prepared; engine should NOT call Capture for a, only for b.
	s = stepN(t, ctx, s, deps, 4) // Pending→Preparing, prep a, prep b, Prepared
	if s.Phase != PhasePrepared {
		t.Fatalf("Phase = %q, want Prepared", s.Phase)
	}

	// Capture was called once for early capture of a, and once by engine for b.
	if len(exec.capturedItems) != 2 || exec.capturedItems[0] != "a" || exec.capturedItems[1] != "b" {
		t.Errorf("captured = %v, want [a b]", exec.capturedItems)
	}
}

func TestCaptureIdempotent(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	deps := Dependencies{Executor: exec, LockManager: &mockLockManager{}}

	s := NewTransaction("tx-1", twoItems(), defaultConfig())

	s, _ = Capture(ctx, s, "a", deps)
	s, _ = Capture(ctx, s, "a", deps)

	if len(exec.capturedItems) != 1 {
		t.Errorf("capture called %d times, want 1 (idempotent)", len(exec.capturedItems))
	}
}

func TestCaptureItemNotFound(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Executor: &mockExecutor{}, LockManager: &mockLockManager{}}

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	_, err := Capture(ctx, s, "nonexistent", deps)
	if !errors.Is(err, ErrItemNotFound) {
		t.Errorf("error = %v, want ErrItemNotFound", err)
	}
}

func TestPrepareLockFailure(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{
		acquire: func(_ context.Context, key, _ string, _ time.Duration) (string, error) {
			if key == "key-a" {
				return "", errors.New("lock contention")
			}
			return "lock-" + key, nil
		},
	}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	s = stepN(t, ctx, s, deps, 1) // → Preparing

	s, result, err := Step(ctx, s, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal=true")
	}
	// No locks acquired (first item failed), so ReleaseAll has nothing to release.
}

func TestPrepareCaptureFailure(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{
		capture: func(_ context.Context, item ItemState) error {
			if item.ID == "a" {
				return errors.New("capture boom")
			}
			return nil
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	s = stepN(t, ctx, s, deps, 1) // → Preparing

	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal=true")
	}
}

func TestPrepareExecutorFailure(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{
		prepare: func(_ context.Context, item ItemState) error {
			if item.ID == "b" {
				return errors.New("prepare boom")
			}
			return nil
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	// Pending→Preparing, prepare a (ok), prepare b (fail).
	s = stepN(t, ctx, s, deps, 3)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed", s.Phase)
	}
}

func TestCommitLockRenewalFailureTriggersRollback(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{
		renew: func(_ context.Context, _, _ string, _ time.Duration) error {
			return errors.New("lease expired")
		},
	}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	// Drive to Committing: Pending(1) + Preparing(3) + Prepared(1) = 5 steps.
	s = stepN(t, ctx, s, deps, 5)
	if s.Phase != PhaseCommitting {
		t.Fatalf("Phase = %q, want Committing", s.Phase)
	}

	// Commit step — lock renewal fails → RollingBack (AutoRollback=true).
	s, _, _ = Step(ctx, s, deps)
	if s.Phase != PhaseRollingBack {
		t.Fatalf("Phase = %q, want RollingBack", s.Phase)
	}
}

func TestCommitPreCommitCheckFailure(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{
		preCommitCheck: func(_ context.Context, _ ItemState) error {
			return errors.New("resource version changed")
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	s = stepN(t, ctx, s, deps, 5) // → Committing

	// PreCommitCheck fails → Failed (no rollback for check failures).
	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
}

func TestCommitForwardSkippableFailsNoRollback(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{
		forward: func(_ context.Context, _ ItemState) error {
			return &Skippable{Err: errors.New("conflict detected")}
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	s = stepN(t, ctx, s, deps, 5) // → Committing

	s, _, _ = Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed (Skippable forward = no rollback)", s.Phase)
	}
}

func TestCommitForwardErrorTriggersRollback(t *testing.T) {
	ctx := context.Background()
	callCount := 0
	exec := &mockExecutor{
		forward: func(_ context.Context, item ItemState) error {
			callCount++
			if item.ID == "b" {
				return errors.New("apply failed")
			}
			return nil
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	s = stepN(t, ctx, s, deps, 5) // → Committing

	// Commit a (ok).
	s = stepN(t, ctx, s, deps, 1)
	if !s.Items[0].Committed {
		t.Fatal("item a should be committed")
	}

	// Commit b (fail) → RollingBack.
	s, _, _ = Step(ctx, s, deps)
	if s.Phase != PhaseRollingBack {
		t.Fatalf("Phase = %q, want RollingBack", s.Phase)
	}
}

func TestCommitForwardErrorNoAutoRollback(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{
		forward: func(_ context.Context, _ ItemState) error {
			return errors.New("apply failed")
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	cfg := defaultConfig()
	cfg.AutoRollback = false
	s := NewTransaction("tx-1", twoItems(), cfg)
	s = stepN(t, ctx, s, deps, 5) // → Committing

	s, _, _ = Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed (AutoRollback=false)", s.Phase)
	}
}

func TestRollbackReverseOrder(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{
		forward: func(_ context.Context, item ItemState) error {
			if item.ID == "b" {
				return errors.New("fail b")
			}
			return nil
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	s := NewTransaction("tx-1", twoItems(), defaultConfig())
	// Drive to Committing, commit a, fail on b → RollingBack.
	s = stepN(t, ctx, s, deps, 7)
	if s.Phase != PhaseRollingBack {
		t.Fatalf("Phase = %q, want RollingBack", s.Phase)
	}

	// Rollback should reverse item a (the only committed item).
	// b was never committed, so it's skipped by the RollingBack loop.
	exec.reversedItems = nil
	s, result, _ := Step(ctx, s, deps)
	if len(exec.reversedItems) != 1 || exec.reversedItems[0] != "a" {
		t.Errorf("reversed = %v, want [a]", exec.reversedItems)
	}

	// Next step: all done → RolledBack.
	s, result, _ = Step(ctx, s, deps)
	if s.Phase != PhaseRolledBack {
		t.Fatalf("Phase = %q, want RolledBack", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
}

func TestRollbackMultipleItemsReverseOrder(t *testing.T) {
	ctx := context.Background()

	items := []ItemState{
		{ID: "x", Order: 1, LockKey: "k-x"},
		{ID: "y", Order: 2, LockKey: "k-y"},
		{ID: "z", Order: 3, LockKey: "k-z"},
	}
	// Start in RollingBack with all items committed.
	now := time.Now()
	s := State{
		Phase:     PhaseRollingBack,
		Holder:    "tx-1",
		Items:     items,
		Config:    defaultConfig(),
		StartedAt: &now,
	}
	for i := range s.Items {
		s.Items[i].Committed = true
		s.Items[i].LockRef = fmt.Sprintf("lock-%s", s.Items[i].ID)
	}

	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	// 3 reverse steps + terminal step.
	s = stepN(t, ctx, s, deps, 4)
	if s.Phase != PhaseRolledBack {
		t.Fatalf("Phase = %q, want RolledBack", s.Phase)
	}

	// Should be in reverse order: z, y, x.
	want := []string{"z", "y", "x"}
	if len(exec.reversedItems) != 3 {
		t.Fatalf("reversed = %v, want %v", exec.reversedItems, want)
	}
	for i, id := range want {
		if exec.reversedItems[i] != id {
			t.Errorf("reversed[%d] = %q, want %q", i, exec.reversedItems[i], id)
		}
	}
}

func TestRollbackSkippable(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"
	items[1].Committed = true
	items[1].LockRef = "lock-b"

	s := State{
		Phase: PhaseRollingBack, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &now,
	}

	exec := &mockExecutor{
		reverse: func(_ context.Context, item ItemState) error {
			if item.ID == "b" {
				return &Skippable{Err: errors.New("conflict")}
			}
			return nil
		},
	}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	// Step 1: reverse b (highest order) → skippable.
	s, _, _ = Step(ctx, s, deps)
	if !s.Items[1].Skipped {
		t.Error("item b should be Skipped")
	}
	if s.Items[1].Error == "" {
		t.Error("item b should have an Error set")
	}

	// Step 2: reverse a → success.
	s, _, _ = Step(ctx, s, deps)

	// Step 3: terminal — has unrolled commits (b is Committed && !RolledBack) → Failed.
	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed (has unresolved skipped items)", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
}

func TestRollbackRetrySignalTrue(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"

	s := State{
		Phase: PhaseRollingBack, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &now,
	}

	callCount := 0
	exec := &mockExecutor{
		reverse: func(_ context.Context, _ ItemState) error {
			callCount++
			if callCount <= 2 {
				return errors.New("transient")
			}
			return nil
		},
	}
	locks := &mockLockManager{}
	deps := Dependencies{
		Executor:    exec,
		LockManager: locks,
		RetrySignal: &mockRetrySignal{shouldRetry: true},
	}

	// Step 1: reverse a → error, retry signal true → RequeueAfter.
	s, result, _ := Step(ctx, s, deps)
	if result.RequeueAfter == 0 {
		t.Error("expected nonzero RequeueAfter")
	}
	if s.Items[0].RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", s.Items[0].RetryCount)
	}

	// Step 2: retry → error again, retry signal true → RequeueAfter (larger).
	s, result, _ = Step(ctx, s, deps)
	if result.RequeueAfter <= 2*time.Second {
		t.Errorf("expected backoff increase, got %v", result.RequeueAfter)
	}
	if s.Items[0].RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", s.Items[0].RetryCount)
	}

	// Step 3: retry → success.
	s, _, _ = Step(ctx, s, deps)
	if !s.Items[0].RolledBack {
		t.Error("item a should be RolledBack after successful retry")
	}
	if s.Items[0].Error != "" {
		t.Error("error should be cleared after successful rollback")
	}
}

func TestRollbackRetrySignalFalse(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"

	s := State{
		Phase: PhaseRollingBack, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &now,
	}

	exec := &mockExecutor{
		reverse: func(_ context.Context, _ ItemState) error {
			return errors.New("permanent")
		},
	}
	locks := &mockLockManager{}
	deps := Dependencies{
		Executor:    exec,
		LockManager: locks,
		RetrySignal: &mockRetrySignal{shouldRetry: false},
	}

	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed (RetrySignal=false)", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
}

func TestRollbackNoRetrySignal(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"

	s := State{
		Phase: PhaseRollingBack, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &now,
	}

	exec := &mockExecutor{
		reverse: func(_ context.Context, _ ItemState) error {
			return errors.New("fail")
		},
	}
	locks := &mockLockManager{}
	deps := Dependencies{Executor: exec, LockManager: locks} // nil RetrySignal

	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed (nil RetrySignal)", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
}

func TestRollbackForgetPerItem(t *testing.T) {
	ctx := context.Background()

	now := time.Now()
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"
	items[1].Committed = true
	items[1].LockRef = "lock-b"

	s := State{
		Phase: PhaseRollingBack, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &now,
	}

	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	// Step 1: reverse b.
	s, _, _ = Step(ctx, s, deps)
	if len(exec.forgottenItems) != 1 || exec.forgottenItems[0] != "b" {
		t.Errorf("forgotten after step 1 = %v, want [b]", exec.forgottenItems)
	}

	// Step 2: reverse a.
	s, _, _ = Step(ctx, s, deps)
	if len(exec.forgottenItems) != 2 || exec.forgottenItems[1] != "a" {
		t.Errorf("forgotten after step 2 = %v, want [b a]", exec.forgottenItems)
	}
}

func TestTimeoutNoCommits(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	past := time.Now().Add(-10 * time.Minute)
	s := State{
		Phase: PhasePreparing, Holder: "tx-1",
		Items: twoItems(), Config: defaultConfig(), StartedAt: &past,
	}

	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
	if s.Error == "" {
		t.Error("expected Error to be set")
	}
}

func TestTimeoutWithCommits(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	past := time.Now().Add(-10 * time.Minute)
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"

	s := State{
		Phase: PhaseCommitting, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &past,
	}

	s, _, _ = Step(ctx, s, deps)
	if s.Phase != PhaseRollingBack {
		t.Fatalf("Phase = %q, want RollingBack (timeout with commits)", s.Phase)
	}
	// StartedAt should be reset for fresh rollback window.
	if s.StartedAt.Before(time.Now().Add(-time.Second)) {
		t.Error("StartedAt should be reset to ~now for rollback window")
	}
}

func TestTimeoutDuringRollback(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	past := time.Now().Add(-10 * time.Minute)
	items := twoItems()
	items[0].Committed = true
	items[0].LockRef = "lock-a"

	s := State{
		Phase: PhaseRollingBack, Holder: "tx-1",
		Items: items, Config: defaultConfig(), StartedAt: &past,
	}

	s, result, _ := Step(ctx, s, deps)
	if s.Phase != PhaseFailed {
		t.Fatalf("Phase = %q, want Failed (rollback timeout)", s.Phase)
	}
	if !result.Terminal {
		t.Error("expected Terminal")
	}
}

func TestTerminalPhaseReturnsTerminal(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Executor: &mockExecutor{}, LockManager: &mockLockManager{}}

	for _, phase := range []Phase{PhaseCommitted, PhaseRolledBack, PhaseFailed} {
		s := State{Phase: phase, Holder: "h", Items: twoItems()}
		s, result, err := Step(ctx, s, deps)
		if err != nil {
			t.Errorf("Phase %s: unexpected error: %v", phase, err)
		}
		if !result.Terminal {
			t.Errorf("Phase %s: expected Terminal=true", phase)
		}
	}
}

func TestImmutability(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Executor: &mockExecutor{}, LockManager: &mockLockManager{}}

	original := NewTransaction("tx-1", twoItems(), defaultConfig())

	// Deep copy for comparison.
	origPhase := original.Phase
	origHolder := original.Holder
	origItemCount := len(original.Items)
	origItem0ID := original.Items[0].ID

	s, _, err := Step(ctx, original, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Step should have advanced the state.
	if s.Phase == origPhase {
		t.Error("returned state should have changed phase")
	}

	// Original should be untouched.
	if original.Phase != origPhase {
		t.Errorf("original Phase changed: %q → %q", origPhase, original.Phase)
	}
	if original.Holder != origHolder {
		t.Error("original Holder changed")
	}
	if len(original.Items) != origItemCount {
		t.Error("original Items length changed")
	}
	if original.Items[0].ID != origItem0ID {
		t.Error("original Items[0].ID changed")
	}
	if original.StartedAt != nil {
		t.Error("original StartedAt should still be nil")
	}
}

func TestImmutabilityCapture(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Executor: &mockExecutor{}, LockManager: &mockLockManager{}}

	original := NewTransaction("tx-1", twoItems(), defaultConfig())

	s, err := Capture(ctx, original, "a", deps)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if s.Items[0].Captured != true {
		t.Error("returned state should mark item captured")
	}
	if original.Items[0].Captured {
		t.Error("original Items[0].Captured should be false")
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		retry int
		want  time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second}, // capped
		{10, 30 * time.Second},
	}
	for _, tt := range tests {
		got := backoff(tt.retry)
		if got != tt.want {
			t.Errorf("backoff(%d) = %v, want %v", tt.retry, got, tt.want)
		}
	}
}

func TestUnknownPhase(t *testing.T) {
	ctx := context.Background()
	deps := Dependencies{Executor: &mockExecutor{}, LockManager: &mockLockManager{}}

	s := State{Phase: "Bogus", Holder: "h", Items: twoItems()}
	_, _, err := Step(ctx, s, deps)
	if err == nil {
		t.Fatal("expected error for unknown phase")
	}
	var inv *ErrInvalidState
	if !errors.As(err, &inv) {
		t.Fatalf("expected ErrInvalidState, got %T: %v", err, err)
	}
}

func TestZeroTimeoutNoEnforcement(t *testing.T) {
	ctx := context.Background()
	exec := &mockExecutor{}
	locks := &mockLockManager{}
	deps := defaultDeps(exec, locks)

	cfg := defaultConfig()
	cfg.Timeout = 0 // no timeout
	past := time.Now().Add(-1 * time.Hour)

	s := State{
		Phase: PhasePreparing, Holder: "tx-1",
		Items: twoItems(), Config: cfg, StartedAt: &past,
	}

	// Should not timeout — zero means disabled.
	s, _, err := Step(ctx, s, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Phase == PhaseFailed {
		t.Error("should not have timed out with Timeout=0")
	}
}

func TestIsSkippable(t *testing.T) {
	skip := &Skippable{Err: errors.New("oops")}
	if !IsSkippable(skip) {
		t.Error("IsSkippable should return true for *Skippable")
	}
	if IsSkippable(errors.New("normal")) {
		t.Error("IsSkippable should return false for normal error")
	}
	wrapped := fmt.Errorf("wrapping: %w", skip)
	if !IsSkippable(wrapped) {
		t.Error("IsSkippable should return true for wrapped *Skippable")
	}
}
