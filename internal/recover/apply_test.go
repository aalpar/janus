package recover

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aalpar/janus/internal/rollback"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

// priorStateCM returns a JSON-serialized ConfigMap suitable for use as PriorState.
func priorStateCM(name, dataVal string) json.RawMessage {
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "ns-1",
			"resourceVersion": "should-be-stripped",
			"uid":             "should-be-stripped",
		},
		"data": map[string]string{"key": dataVal},
	}
	b, _ := json.Marshal(cm)
	return b
}

func TestApplyItem_Delete(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cm-created",
			Namespace: "ns-1",
		},
		Data: map[string]string{"key": "val"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()

	rbKey := rollback.Key("ConfigMap", "ns-1", "cm-created")
	rbCM := &corev1.ConfigMap{
		Data: map[string]string{
			rbKey: mustMarshal(rollback.Envelope{ChangeType: "Create"}),
		},
	}

	item := PlanItem{
		Target:    rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-created", Namespace: "ns-1"},
		Operation: "DELETE",
	}

	if err := ApplyItem(ctx, cl, item, rbCM, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyItem DELETE: %v", err)
	}

	// Verify deleted.
	got := &corev1.ConfigMap{}
	err := cl.Get(ctx, client.ObjectKey{Name: "cm-created", Namespace: "ns-1"}, got)
	if err == nil {
		t.Fatal("expected NotFound after DELETE, but resource still exists")
	}
}

func TestApplyItem_DeleteAlreadyGone(t *testing.T) {
	ctx := context.Background()
	// No objects in the fake client.
	cl := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	rbKey := rollback.Key("ConfigMap", "ns-1", "cm-gone")
	rbCM := &corev1.ConfigMap{
		Data: map[string]string{
			rbKey: mustMarshal(rollback.Envelope{ChangeType: "Create"}),
		},
	}

	item := PlanItem{
		Target:    rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-gone", Namespace: "ns-1"},
		Operation: "DELETE",
	}

	if err := ApplyItem(ctx, cl, item, rbCM, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyItem DELETE on missing resource should be no-op, got: %v", err)
	}
}

func TestApplyItem_Restore(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "cm-patched",
			Namespace:       "ns-1",
			ResourceVersion: "200",
		},
		Data: map[string]string{"key": "new-value"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()

	rbKey := rollback.Key("ConfigMap", "ns-1", "cm-patched")
	rbCM := &corev1.ConfigMap{
		Data: map[string]string{
			rbKey: mustMarshal(rollback.Envelope{
				ResourceVersion: "200",
				ChangeType:      "Patch",
				PriorState:      priorStateCM("cm-patched", "old-value"),
			}),
		},
	}

	item := PlanItem{
		Target:    rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-patched", Namespace: "ns-1"},
		Operation: "RESTORE",
		StoredRV:  "200",
	}

	if err := ApplyItem(ctx, cl, item, rbCM, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyItem RESTORE: %v", err)
	}

	// Verify the data was restored.
	got := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "cm-patched", Namespace: "ns-1"}, got); err != nil {
		t.Fatalf("Get after RESTORE: %v", err)
	}
	if got.Data["key"] != "old-value" {
		t.Errorf("expected data[key]=old-value, got %q", got.Data["key"])
	}
}

func TestApplyItem_RestoreNotFound(t *testing.T) {
	ctx := context.Background()
	// Resource doesn't exist — it was deleted externally. RESTORE should recreate it.
	cl := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	rbKey := rollback.Key("ConfigMap", "ns-1", "cm-deleted")
	rbCM := &corev1.ConfigMap{
		Data: map[string]string{
			rbKey: mustMarshal(rollback.Envelope{
				ResourceVersion: "150",
				ChangeType:      "Delete",
				PriorState:      priorStateCM("cm-deleted", "restored"),
			}),
		},
	}

	item := PlanItem{
		Target:    rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-deleted", Namespace: "ns-1"},
		Operation: "RESTORE",
		StoredRV:  "150",
	}

	if err := ApplyItem(ctx, cl, item, rbCM, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyItem RESTORE (not found): %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "cm-deleted", Namespace: "ns-1"}, got); err != nil {
		t.Fatalf("resource should exist after RESTORE create: %v", err)
	}
	if got.Data["key"] != "restored" {
		t.Errorf("expected data[key]=restored, got %q", got.Data["key"])
	}
}

func TestApplyItem_ConflictRefused(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "cm-conflict",
			Namespace:       "ns-1",
			ResourceVersion: "999", // current RV differs from stored
		},
		Data: map[string]string{"key": "current"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()

	rbKey := rollback.Key("ConfigMap", "ns-1", "cm-conflict")
	rbCM := &corev1.ConfigMap{
		Data: map[string]string{
			rbKey: mustMarshal(rollback.Envelope{
				ResourceVersion: "100", // stored RV != current 999
				ChangeType:      "Patch",
				PriorState:      priorStateCM("cm-conflict", "old"),
			}),
		},
	}

	item := PlanItem{
		Target:    rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-conflict", Namespace: "ns-1"},
		Operation: "RESTORE",
		StoredRV:  "100",
	}

	err := ApplyItem(ctx, cl, item, rbCM, ApplyOptions{Force: false})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	var conflict *ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *ErrConflict, got %T: %v", err, err)
	}
	if conflict.StoredRV != "100" || conflict.CurrentRV != "999" {
		t.Errorf("conflict RVs: stored=%s current=%s", conflict.StoredRV, conflict.CurrentRV)
	}
}

func TestApplyItem_ConflictForced(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "cm-force",
			Namespace:       "ns-1",
			ResourceVersion: "999",
		},
		Data: map[string]string{"key": "current"},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()

	rbKey := rollback.Key("ConfigMap", "ns-1", "cm-force")
	rbCM := &corev1.ConfigMap{
		Data: map[string]string{
			rbKey: mustMarshal(rollback.Envelope{
				ResourceVersion: "100", // mismatch, but Force=true
				ChangeType:      "Patch",
				PriorState:      priorStateCM("cm-force", "old"),
			}),
		},
	}

	item := PlanItem{
		Target:    rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-force", Namespace: "ns-1"},
		Operation: "RESTORE",
		StoredRV:  "100",
	}

	if err := ApplyItem(ctx, cl, item, rbCM, ApplyOptions{Force: true}); err != nil {
		t.Fatalf("ApplyItem RESTORE with Force: %v", err)
	}

	got := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "cm-force", Namespace: "ns-1"}, got); err != nil {
		t.Fatalf("Get after forced RESTORE: %v", err)
	}
	if got.Data["key"] != "old" {
		t.Errorf("expected data[key]=old after force, got %q", got.Data["key"])
	}
}
