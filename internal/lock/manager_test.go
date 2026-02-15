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
	"errors"
	"fmt"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(s)
	return s
}

func newManager(objs ...client.Object) *LeaseManager {
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		Build()
	return &LeaseManager{Client: c}
}

func newManagerWithInterceptors(funcs interceptor.Funcs, objs ...client.Object) *LeaseManager {
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
	return &LeaseManager{Client: c}
}

const (
	txnOwner = "txn-1"
	txnOther = "txn-other"
)

var (
	testKey = ResourceKey{Namespace: "default", Kind: "ConfigMap", Name: "my-cm"}
	testCtx = context.Background()
)

// --- LeaseName ---

func TestLeaseName_Deterministic(t *testing.T) {
	name := LeaseName(testKey)
	if name != "janus-lock-default-configmap-my-cm" {
		t.Fatalf("unexpected lease name: %s", name)
	}
}

func TestLeaseName_Lowercases(t *testing.T) {
	key := ResourceKey{Namespace: "MyNS", Kind: "Deployment", Name: "MyApp"}
	name := LeaseName(key)
	if name != "janus-lock-myns-deployment-myapp" {
		t.Fatalf("expected lowercased name, got: %s", name)
	}
}

// --- Acquire ---

func TestAcquire_CreatesNewLease(t *testing.T) {
	m := newManager()
	name, err := m.Acquire(testCtx, testKey, txnOwner, 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != LeaseName(testKey) {
		t.Fatalf("unexpected name: %s", name)
	}

	// Verify lease was created.
	lease := &coordinationv1.Lease{}
	if err := m.Client.Get(testCtx, client.ObjectKey{Name: name, Namespace: "default"}, lease); err != nil {
		t.Fatalf("lease not found: %v", err)
	}
	if *lease.Spec.HolderIdentity != txnOwner {
		t.Fatalf("unexpected holder: %s", *lease.Spec.HolderIdentity)
	}
	if lease.Labels["janus.io/transaction"] != txnOwner {
		t.Fatalf("missing transaction label")
	}
}

func TestAcquire_RenewsExistingLease(t *testing.T) {
	holder := txnOwner
	dur := int32(300)
	now := metav1.NewMicroTime(time.Now())
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaseName(testKey),
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         holder,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	m := newManager(existing)

	name, err := m.Acquire(testCtx, testKey, txnOwner, 10*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != LeaseName(testKey) {
		t.Fatalf("unexpected name: %s", name)
	}

	// Verify duration was updated.
	lease := &coordinationv1.Lease{}
	_ = m.Client.Get(testCtx, client.ObjectKey{Name: name, Namespace: "default"}, lease)
	if *lease.Spec.LeaseDurationSeconds != 600 {
		t.Fatalf("expected 600s, got %d", *lease.Spec.LeaseDurationSeconds)
	}
}

func TestAcquire_RejectsWhenHeldByDifferentTxn(t *testing.T) {
	holder := txnOther
	dur := int32(3600) // Far from expired.
	now := metav1.NewMicroTime(time.Now())
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaseName(testKey),
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         holder,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	m := newManager(existing)

	_, err := m.Acquire(testCtx, testKey, txnOwner, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error")
	}
	var alErr *ErrAlreadyLocked
	if !errors.As(err, &alErr) {
		t.Fatalf("expected ErrAlreadyLocked, got: %v", err)
	}
	if alErr.Holder != txnOther {
		t.Fatalf("unexpected holder in error: %s", alErr.Holder)
	}
}

func TestAcquire_TakesOverExpiredLease(t *testing.T) {
	holder := "txn-old"
	dur := int32(1) // 1 second.
	past := metav1.NewMicroTime(time.Now().Add(-10 * time.Second))
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaseName(testKey),
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         holder,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &past,
			RenewTime:            &past,
		},
	}
	m := newManager(existing)

	name, err := m.Acquire(testCtx, testKey, "txn-new", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lease := &coordinationv1.Lease{}
	_ = m.Client.Get(testCtx, client.ObjectKey{Name: name, Namespace: "default"}, lease)
	if *lease.Spec.HolderIdentity != "txn-new" {
		t.Fatalf("expected txn-new, got %s", *lease.Spec.HolderIdentity)
	}
	if lease.Labels["janus.io/transaction"] != "txn-new" {
		t.Fatalf("label not updated")
	}
}

func TestAcquire_GetError(t *testing.T) {
	m := newManagerWithInterceptors(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return fmt.Errorf("synthetic get error")
		},
	})

	_, err := m.Acquire(testCtx, testKey, txnOwner, 5*time.Minute)
	if err == nil || err.Error() != fmt.Sprintf("getting lease %s: synthetic get error", LeaseName(testKey)) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquire_CreateError(t *testing.T) {
	m := newManagerWithInterceptors(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return fmt.Errorf("synthetic create error")
		},
	})

	_, err := m.Acquire(testCtx, testKey, txnOwner, 5*time.Minute)
	if err == nil || err.Error() != fmt.Sprintf("creating lease %s: synthetic create error", LeaseName(testKey)) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquire_RenewError(t *testing.T) {
	holder := txnOwner
	dur := int32(300)
	now := metav1.NewMicroTime(time.Now())
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LeaseName(testKey),
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         holder,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	m := newManagerWithInterceptors(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return fmt.Errorf("synthetic update error")
		},
	}, existing)

	_, err := m.Acquire(testCtx, testKey, txnOwner, 5*time.Minute)
	if err == nil || err.Error() != fmt.Sprintf("renewing lease %s: synthetic update error", LeaseName(testKey)) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Release ---

func TestRelease_DeletesExistingLease(t *testing.T) {
	name := LeaseName(testKey)
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
			},
		},
	}
	m := newManager(existing)

	if err := m.Release(testCtx, name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it's gone.
	leaseList := &coordinationv1.LeaseList{}
	_ = m.Client.List(testCtx, leaseList)
	if len(leaseList.Items) != 0 {
		t.Fatalf("expected 0 leases, got %d", len(leaseList.Items))
	}
}

func TestRelease_NoOpWhenAlreadyGone(t *testing.T) {
	m := newManager()
	if err := m.Release(testCtx, "nonexistent-lease"); err != nil {
		t.Fatalf("expected no error for missing lease, got: %v", err)
	}
}

func TestRelease_ListError(t *testing.T) {
	m := newManagerWithInterceptors(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return fmt.Errorf("synthetic list error")
		},
	})

	err := m.Release(testCtx, "some-lease")
	if err == nil || err.Error() != "listing leases for release: synthetic list error" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRelease_DeleteError(t *testing.T) {
	name := LeaseName(testKey)
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
			},
		},
	}
	m := newManagerWithInterceptors(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return fmt.Errorf("synthetic delete error")
		},
	}, existing)

	err := m.Release(testCtx, name)
	if err == nil || err.Error() != fmt.Sprintf("deleting lease %s: synthetic delete error", name) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- ReleaseAll ---

func TestReleaseAll_MultipleLeasesReleased(t *testing.T) {
	key2 := ResourceKey{Namespace: "default", Kind: "Secret", Name: "my-secret"}
	name1 := LeaseName(testKey)
	name2 := LeaseName(key2)
	leases := []client.Object{
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name1, Namespace: "default",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "janus"},
			},
		},
		&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name2, Namespace: "default",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "janus"},
			},
		},
	}
	m := newManager(leases...)

	err := m.ReleaseAll(testCtx, txnOwner, []string{name1, name2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	leaseList := &coordinationv1.LeaseList{}
	_ = m.Client.List(testCtx, leaseList)
	if len(leaseList.Items) != 0 {
		t.Fatalf("expected 0 leases, got %d", len(leaseList.Items))
	}
}

func TestReleaseAll_SkipsEmptyNames(t *testing.T) {
	m := newManager()
	err := m.ReleaseAll(testCtx, txnOwner, []string{"", "", ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReleaseAll_ReturnsFirstError(t *testing.T) {
	m := newManagerWithInterceptors(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return fmt.Errorf("list error")
		},
	})

	err := m.ReleaseAll(testCtx, txnOwner, []string{"lease-a", "lease-b"})
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "listing leases for release: list error" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- IsHeldBy ---

func TestIsHeldBy_HeldByCorrectTxn(t *testing.T) {
	holder := txnOwner
	dur := int32(3600)
	now := metav1.NewMicroTime(time.Now())
	name := LeaseName(testKey)
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         holder,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			RenewTime:            &now,
		},
	}
	m := newManager(existing)

	held, err := m.IsHeldBy(testCtx, name, txnOwner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !held {
		t.Fatal("expected held=true")
	}
}

func TestIsHeldBy_HeldByDifferentTxn(t *testing.T) {
	holder := txnOther
	dur := int32(3600)
	now := metav1.NewMicroTime(time.Now())
	name := LeaseName(testKey)
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         txnOwner, // Label says txn-1 but holder is txn-other.
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			RenewTime:            &now,
		},
	}
	m := newManager(existing)

	held, err := m.IsHeldBy(testCtx, name, txnOwner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if held {
		t.Fatal("expected held=false")
	}
}

func TestIsHeldBy_LeaseNotFound(t *testing.T) {
	m := newManager()

	held, err := m.IsHeldBy(testCtx, "nonexistent", txnOwner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if held {
		t.Fatal("expected held=false")
	}
}

func TestIsHeldBy_LeaseExpired(t *testing.T) {
	holder := txnOwner
	dur := int32(1)
	past := metav1.NewMicroTime(time.Now().Add(-10 * time.Second))
	name := LeaseName(testKey)
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         holder,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			LeaseDurationSeconds: &dur,
			RenewTime:            &past,
		},
	}
	m := newManager(existing)

	held, err := m.IsHeldBy(testCtx, name, txnOwner)
	if held {
		t.Fatal("expected held=false")
	}
	var expErr *ErrLockExpired
	if !errors.As(err, &expErr) {
		t.Fatalf("expected ErrLockExpired, got: %v", err)
	}
}

func TestIsHeldBy_ListError(t *testing.T) {
	m := newManagerWithInterceptors(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return fmt.Errorf("list error")
		},
	})

	_, err := m.IsHeldBy(testCtx, "some-lease", txnOwner)
	if err == nil || err.Error() != "listing leases: list error" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- isExpired ---

func TestIsExpired_NilRenewTime(t *testing.T) {
	m := &LeaseManager{}
	dur := int32(300)
	lease := &coordinationv1.Lease{
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: &dur,
			RenewTime:            nil,
		},
	}
	if !m.isExpired(lease) {
		t.Fatal("expected expired when RenewTime is nil")
	}
}

func TestIsExpired_NilDuration(t *testing.T) {
	m := &LeaseManager{}
	now := metav1.NewMicroTime(time.Now())
	lease := &coordinationv1.Lease{
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: nil,
			RenewTime:            &now,
		},
	}
	if !m.isExpired(lease) {
		t.Fatal("expected expired when LeaseDurationSeconds is nil")
	}
}

func TestIsExpired_NotExpired(t *testing.T) {
	m := &LeaseManager{}
	dur := int32(3600)
	now := metav1.NewMicroTime(time.Now())
	lease := &coordinationv1.Lease{
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: &dur,
			RenewTime:            &now,
		},
	}
	if m.isExpired(lease) {
		t.Fatal("expected not expired")
	}
}

func TestIsExpired_Expired(t *testing.T) {
	m := &LeaseManager{}
	dur := int32(1)
	past := metav1.NewMicroTime(time.Now().Add(-10 * time.Second))
	lease := &coordinationv1.Lease{
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: &dur,
			RenewTime:            &past,
		},
	}
	if !m.isExpired(lease) {
		t.Fatal("expected expired")
	}
}

// --- Error types ---

func TestErrAlreadyLocked_Error(t *testing.T) {
	err := &ErrAlreadyLocked{LeaseName: "lease-1", Holder: txnOther}
	expected := `resource is locked by txnOther (lease lease-1)`
	if err.Error() != expected {
		t.Fatalf("unexpected: %s", err.Error())
	}
}

func TestErrLockExpired_Error(t *testing.T) {
	err := &ErrLockExpired{LeaseName: "lease-1"}
	expected := "lock lease lease-1 has expired"
	if err.Error() != expected {
		t.Fatalf("unexpected: %s", err.Error())
	}
}

func TestErrAlreadyLocked_ErrorsAs(t *testing.T) {
	var err error = &ErrAlreadyLocked{LeaseName: "l", Holder: "h"}
	var target *ErrAlreadyLocked
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed for ErrAlreadyLocked")
	}
}

func TestErrLockExpired_ErrorsAs(t *testing.T) {
	var err error = &ErrLockExpired{LeaseName: "l"}
	var target *ErrLockExpired
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed for ErrLockExpired")
	}
}
