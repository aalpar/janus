package saga

import "time"

// Phase represents the current state of a saga transaction.
type Phase string

const (
	PhasePending     Phase = "Pending"
	PhasePreparing   Phase = "Preparing"
	PhasePrepared    Phase = "Prepared"
	PhaseCommitting  Phase = "Committing"
	PhaseCommitted   Phase = "Committed"
	PhaseRollingBack Phase = "RollingBack"
	PhaseRolledBack  Phase = "RolledBack"
	PhaseFailed      Phase = "Failed"
)

func isTerminal(p Phase) bool {
	return p == PhaseCommitted || p == PhaseRolledBack || p == PhaseFailed
}

// ItemID is a unique identifier for a transaction item.
type ItemID = string

// State is the complete state of a saga transaction.
// Passed to and returned from every engine call — the engine is stateless.
type State struct {
	Phase       Phase
	Holder      string // transaction identity for lock ownership
	Items       []ItemState
	Config      Config
	StartedAt   *time.Time
	CompletedAt *time.Time
	Error       string // transaction-level failure reason; empty unless Failed
}

// ItemState tracks the progress of a single step within the transaction.
type ItemState struct {
	ID         ItemID
	Order      int    // sort key: (Order, ID) forward; reverse for rollback
	LockKey    string // opaque, passed to LockManager.Acquire
	LockRef    string // opaque, returned by LockManager.Acquire
	Captured   bool
	Prepared   bool
	Committed  bool
	RolledBack bool
	Skipped    bool // set when Reverse returns Skippable; needs manual resolution
	Error      string
	RetryCount int
}

// Config holds transaction-level settings.
type Config struct {
	Timeout      time.Duration // global deadline
	LockTimeout  time.Duration // per-lock expiry
	AutoRollback bool          // rollback on forward failure
}

// Result tells the caller what to do next.
type Result struct {
	Terminal     bool
	RequeueAfter time.Duration
}
