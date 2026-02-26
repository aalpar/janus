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

package lock

import (
	"context"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ResourceKey identifies a Kubernetes resource for locking purposes.
type ResourceKey struct {
	Namespace string
	Kind      string
	Name      string
}

// LeaseRef identifies a Lease object by name and namespace.
type LeaseRef struct {
	Name      string
	Namespace string
}

// Manager manages Lease-based advisory locks for transaction resources.
type Manager interface {
	// Acquire creates or takes over a Lease lock for the given resource.
	// Returns the lease name on success.
	Acquire(ctx context.Context, key ResourceKey, txnName string, timeout time.Duration) (string, error)

	// Release deletes a single Lease lock if held by the given transaction.
	// If the lease is held by a different transaction, the call is a no-op.
	Release(ctx context.Context, lease LeaseRef, txnName string) error

	// ReleaseAll releases multiple leases, returning the first error encountered.
	ReleaseAll(ctx context.Context, leases []LeaseRef, txnName string) error

	// Renew extends the lease duration for a lock held by the given transaction.
	// Returns an error if the lease is not found, expired, or held by a different transaction.
	Renew(ctx context.Context, lease LeaseRef, txnName string, timeout time.Duration) error
}

// LeaseManager implements Manager using coordination.k8s.io/v1 Lease objects.
type LeaseManager struct {
	Client client.Client
}

// LeaseName returns the deterministic lease name for a resource.
func LeaseName(key ResourceKey) string {
	return fmt.Sprintf("janus-lock-%s-%s-%s",
		strings.ToLower(key.Namespace),
		strings.ToLower(key.Kind),
		strings.ToLower(key.Name),
	)
}

// holderOf returns the holder identity of a lease, or empty string if unset.
func holderOf(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity != nil {
		return *lease.Spec.HolderIdentity
	}
	return ""
}

// setLeaseTime sets the renew time and lease duration on a lease.
// Returns the timestamp used so callers can reuse it (e.g. for AcquireTime).
func setLeaseTime(lease *coordinationv1.Lease, timeout time.Duration) metav1.MicroTime {
	now := metav1.NewMicroTime(time.Now())
	durationSec := int32(timeout.Seconds())
	lease.Spec.RenewTime = &now
	lease.Spec.LeaseDurationSeconds = &durationSec
	return now
}

func (m *LeaseManager) createLease(ctx context.Context, key ResourceKey, name, txnName string, timeout time.Duration) (string, error) {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         txnName,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &txnName,
		},
	}
	now := setLeaseTime(lease, timeout)
	lease.Spec.AcquireTime = &now
	if err := m.Client.Create(ctx, lease); err != nil {
		return "", &LeaseOpError{Op: "creating", LeaseName: name, Err: err}
	}
	return name, nil
}

func (m *LeaseManager) Acquire(ctx context.Context, key ResourceKey, txnName string, timeout time.Duration) (string, error) {
	name := LeaseName(key)

	lease := &coordinationv1.Lease{}
	err := m.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: key.Namespace}, lease)

	if apierrors.IsNotFound(err) {
		return m.createLease(ctx, key, name, txnName, timeout)
	}
	if err != nil {
		return "", &LeaseOpError{Op: "getting", LeaseName: name, Err: err}
	}

	holder := holderOf(lease)

	if holder == txnName {
		// Already held by this transaction — renew.
		setLeaseTime(lease, timeout)
		if err := m.Client.Update(ctx, lease); err != nil {
			return "", &LeaseOpError{Op: "renewing", LeaseName: name, Err: err}
		}
		return name, nil
	}

	// Held by a different transaction — check expiry.
	if !isExpired(lease) {
		return "", &ErrAlreadyLocked{LeaseName: name, Holder: holder}
	}

	// Expired — force takeover.
	lease.Spec.HolderIdentity = &txnName
	now := setLeaseTime(lease, timeout)
	lease.Spec.AcquireTime = &now
	lease.Labels["janus.io/transaction"] = txnName
	if err := m.Client.Update(ctx, lease); err != nil {
		return "", &LeaseOpError{Op: "taking over expired", LeaseName: name, Err: err}
	}
	return name, nil
}

func (m *LeaseManager) Release(ctx context.Context, lease LeaseRef, txnName string) error {
	existing := &coordinationv1.Lease{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: lease.Name, Namespace: lease.Namespace}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Already gone.
		}
		return &LeaseOpError{Op: "getting", LeaseName: lease.Name, Err: err}
	}
	if holder := holderOf(existing); holder != "" && holder != txnName {
		return nil // Held by someone else — not our lock.
	}
	if err := m.Client.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return &LeaseOpError{Op: "deleting", LeaseName: lease.Name, Err: err}
	}
	return nil
}

func (m *LeaseManager) ReleaseAll(ctx context.Context, leases []LeaseRef, txnName string) error {
	var firstErr error
	for _, ref := range leases {
		if ref.Name == "" {
			continue
		}
		if err := m.Release(ctx, ref, txnName); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *LeaseManager) Renew(ctx context.Context, lease LeaseRef, txnName string, timeout time.Duration) error {
	existing := &coordinationv1.Lease{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: lease.Name, Namespace: lease.Namespace}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return &ErrLockExpired{LeaseName: lease.Name}
		}
		return &LeaseOpError{Op: "getting", LeaseName: lease.Name, Err: err}
	}

	if holder := holderOf(existing); holder != txnName {
		return &ErrAlreadyLocked{LeaseName: lease.Name, Holder: holder}
	}
	if isExpired(existing) {
		return &ErrLockExpired{LeaseName: lease.Name}
	}

	setLeaseTime(existing, timeout)
	if err := m.Client.Update(ctx, existing); err != nil {
		return &LeaseOpError{Op: "renewing", LeaseName: lease.Name, Err: err}
	}
	return nil
}

func isExpired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return time.Now().After(expiry)
}
