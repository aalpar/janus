package saga

import (
	"errors"
	"fmt"
)

// Skippable wraps an error to signal that an item cannot be completed and
// should be skipped. The engine interprets the error type, not the message.
type Skippable struct {
	Err error
}

func (e *Skippable) Error() string { return e.Err.Error() }
func (e *Skippable) Unwrap() error { return e.Err }

// IsSkippable reports whether err (or any error in its chain) is a Skippable.
func IsSkippable(err error) bool {
	var s *Skippable
	return errors.As(err, &s)
}

// ErrItemNotFound is returned by Capture when the item ID does not exist
// in the transaction's item list.
var ErrItemNotFound = errors.New("item not found")

// ErrInvalidState is returned by Step or Capture when the transaction state
// fails validation (empty holder, no items, unknown phase).
type ErrInvalidState struct {
	Reason string
}

func (e *ErrInvalidState) Error() string {
	return fmt.Sprintf("invalid state: %s", e.Reason)
}
