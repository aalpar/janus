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

	// Release deletes a single Lease lock.
	Release(ctx context.Context, lease LeaseRef) error

	// ReleaseAll releases multiple leases, returning the first error encountered.
	ReleaseAll(ctx context.Context, leases []LeaseRef) error

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

func (m *LeaseManager) Acquire(ctx context.Context, key ResourceKey, txnName string, timeout time.Duration) (string, error) {
	name := LeaseName(key)
	durationSec := int32(timeout.Seconds())
	now := metav1.NewMicroTime(time.Now())

	lease := &coordinationv1.Lease{}
	err := m.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: key.Namespace}, lease)

	if apierrors.IsNotFound(err) {
		// No existing lease — create one.
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: key.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "janus",
					"janus.io/transaction":         txnName,
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &txnName,
				LeaseDurationSeconds: &durationSec,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		if err := m.Client.Create(ctx, lease); err != nil {
			return "", &LeaseOpError{Op: "creating", LeaseName: name, Err: err}
		}
		return name, nil
	}
	if err != nil {
		return "", &LeaseOpError{Op: "getting", LeaseName: name, Err: err}
	}

	// Lease exists — check holder.
	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}

	if holder == txnName {
		// Already held by this transaction — renew.
		lease.Spec.RenewTime = &now
		lease.Spec.LeaseDurationSeconds = &durationSec
		if err := m.Client.Update(ctx, lease); err != nil {
			return "", &LeaseOpError{Op: "renewing", LeaseName: name, Err: err}
		}
		return name, nil
	}

	// Held by a different transaction — check expiry.
	if !m.isExpired(lease) {
		return "", &ErrAlreadyLocked{LeaseName: name, Holder: holder}
	}

	// Expired — force takeover.
	lease.Spec.HolderIdentity = &txnName
	lease.Spec.LeaseDurationSeconds = &durationSec
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	lease.Labels["janus.io/transaction"] = txnName
	if err := m.Client.Update(ctx, lease); err != nil {
		return "", &LeaseOpError{Op: "taking over expired", LeaseName: name, Err: err}
	}
	return name, nil
}

func (m *LeaseManager) Release(ctx context.Context, lease LeaseRef) error {
	existing := &coordinationv1.Lease{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: lease.Name, Namespace: lease.Namespace}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Already gone.
		}
		return &LeaseOpError{Op: "getting", LeaseName: lease.Name, Err: err}
	}
	if err := m.Client.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return &LeaseOpError{Op: "deleting", LeaseName: lease.Name, Err: err}
	}
	return nil
}

func (m *LeaseManager) ReleaseAll(ctx context.Context, leases []LeaseRef) error {
	var firstErr error
	for _, ref := range leases {
		if ref.Name == "" {
			continue
		}
		if err := m.Release(ctx, ref); err != nil && firstErr == nil {
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

	holder := ""
	if existing.Spec.HolderIdentity != nil {
		holder = *existing.Spec.HolderIdentity
	}
	if holder != txnName {
		return &ErrAlreadyLocked{LeaseName: lease.Name, Holder: holder}
	}
	if m.isExpired(existing) {
		return &ErrLockExpired{LeaseName: lease.Name}
	}

	now := metav1.NewMicroTime(time.Now())
	durationSec := int32(timeout.Seconds())
	existing.Spec.RenewTime = &now
	existing.Spec.LeaseDurationSeconds = &durationSec
	if err := m.Client.Update(ctx, existing); err != nil {
		return &LeaseOpError{Op: "renewing", LeaseName: lease.Name, Err: err}
	}
	return nil
}

func (m *LeaseManager) isExpired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return time.Now().After(expiry)
}
