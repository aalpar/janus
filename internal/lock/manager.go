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

// Manager manages Lease-based advisory locks for transaction resources.
type Manager interface {
	// Acquire creates or takes over a Lease lock for the given resource.
	// Returns the lease name on success.
	Acquire(ctx context.Context, key ResourceKey, txnName string, timeout time.Duration) (string, error)

	// Release deletes a single Lease lock.
	Release(ctx context.Context, leaseName string) error

	// ReleaseAll releases multiple leases, returning the first error encountered.
	ReleaseAll(ctx context.Context, txnName string, leaseNames []string) error

	// IsHeldBy checks whether the named Lease is held by the given transaction.
	IsHeldBy(ctx context.Context, leaseName string, txnName string) (bool, error)
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
			return "", fmt.Errorf("creating lease %s: %w", name, err)
		}
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("getting lease %s: %w", name, err)
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
			return "", fmt.Errorf("renewing lease %s: %w", name, err)
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
		return "", fmt.Errorf("taking over expired lease %s: %w", name, err)
	}
	return name, nil
}

func (m *LeaseManager) Release(ctx context.Context, leaseName string) error {
	// We need to find the lease's namespace. List by label since we know the name.
	lease := &coordinationv1.Lease{}
	// Try to find and delete. We list across all namespaces with a field selector isn't
	// available for leases, so we use label-based listing.
	leaseList := &coordinationv1.LeaseList{}
	if err := m.Client.List(ctx, leaseList,
		client.MatchingLabels{"app.kubernetes.io/managed-by": "janus"},
	); err != nil {
		return fmt.Errorf("listing leases for release: %w", err)
	}

	for i := range leaseList.Items {
		if leaseList.Items[i].Name == leaseName {
			lease = &leaseList.Items[i]
			break
		}
	}
	if lease.Name == "" {
		return nil // Already gone.
	}

	if err := m.Client.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting lease %s: %w", leaseName, err)
	}
	return nil
}

func (m *LeaseManager) ReleaseAll(ctx context.Context, txnName string, leaseNames []string) error {
	var firstErr error
	for _, name := range leaseNames {
		if name == "" {
			continue
		}
		if err := m.Release(ctx, name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *LeaseManager) IsHeldBy(ctx context.Context, leaseName string, txnName string) (bool, error) {
	leaseList := &coordinationv1.LeaseList{}
	if err := m.Client.List(ctx, leaseList,
		client.MatchingLabels{
			"app.kubernetes.io/managed-by": "janus",
			"janus.io/transaction":         txnName,
		},
	); err != nil {
		return false, fmt.Errorf("listing leases: %w", err)
	}

	for _, l := range leaseList.Items {
		if l.Name == leaseName {
			if l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == txnName {
				if m.isExpired(&l) {
					return false, &ErrLockExpired{LeaseName: leaseName}
				}
				return true, nil
			}
			return false, nil
		}
	}
	return false, nil
}

func (m *LeaseManager) isExpired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return time.Now().After(expiry)
}
