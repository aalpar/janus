/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"errors"
	"fmt"
)

// Sentinel errors for controller operations.
var (
	errUnknownChangeType = errors.New("unknown change type")
)

// ResourceOpError wraps an error with resource operation context.
type ResourceOpError struct {
	Op  string // e.g. "parsing apiVersion", "fetching for update", "unmarshaling content"
	Ref string // human-readable resource identifier
	Err error
}

func (e *ResourceOpError) Error() string {
	if e.Ref != "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.Ref, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *ResourceOpError) Unwrap() error {
	return e.Err
}

// RollbackDataError indicates missing or corrupt rollback state.
type RollbackDataError struct {
	Key string
	Err error // nil for missing data, non-nil for deserialization failure
}

func (e *RollbackDataError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("deserializing rollback for %s: %v", e.Key, e.Err)
	}
	return fmt.Sprintf("no rollback data for %s", e.Key)
}

func (e *RollbackDataError) Unwrap() error {
	return e.Err
}
