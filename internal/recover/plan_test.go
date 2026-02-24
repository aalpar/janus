package recover

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aalpar/janus/internal/rollback"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// makeRollbackCM builds a minimal rollback ConfigMap for testing.
func makeRollbackCM(meta rollback.Meta, envelopes map[string]rollback.Envelope) *corev1.ConfigMap {
	data := map[string]string{
		rollback.MetaKey: mustMarshal(meta),
	}
	for k, env := range envelopes {
		data[k] = mustMarshal(env)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      meta.TransactionName + "-rollback",
			Namespace: meta.TransactionNamespace,
		},
		Data: data,
	}
}

func TestBuildPlan_PendingRollback(t *testing.T) {
	meta := rollback.Meta{
		Version:              2,
		TransactionName:      "txn-1",
		TransactionNamespace: "ns-1",
		Changes: []rollback.MetaChange{
			{
				Name:        "cm-a-change",
				Target:      rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-a", Namespace: "ns-1"},
				ChangeType:  "Patch",
				RollbackKey: "ConfigMap_ns-1_cm-a",
			},
		},
	}
	envelopes := map[string]rollback.Envelope{
		"ConfigMap_ns-1_cm-a": {
			ResourceVersion: "100",
			ChangeType:      "Patch",
			PriorState:      json.RawMessage(`{"data":{"key":"old"}}`),
		},
	}
	cm := makeRollbackCM(meta, envelopes)

	plan, err := BuildPlan(cm, nil)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(plan.Items))
	}
	item := plan.Items[0]
	if item.Status != StatusPending {
		t.Errorf("expected status pending, got %s", item.Status)
	}
	if item.Operation != "RESTORE" {
		t.Errorf("expected operation RESTORE, got %s", item.Operation)
	}
	if item.StoredRV != "100" {
		t.Errorf("expected storedRV 100, got %s", item.StoredRV)
	}
}

func TestBuildPlan_CreateIsDelete(t *testing.T) {
	meta := rollback.Meta{
		Version:              2,
		TransactionName:      "txn-2",
		TransactionNamespace: "ns-1",
		Changes: []rollback.MetaChange{
			{
				Name:        "cm-new-change",
				Target:      rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-new", Namespace: "ns-1"},
				ChangeType:  "Create",
				RollbackKey: "ConfigMap_ns-1_cm-new",
			},
		},
	}
	// Create envelopes have no PriorState and no ResourceVersion.
	envelopes := map[string]rollback.Envelope{
		"ConfigMap_ns-1_cm-new": {
			ChangeType: "Create",
		},
	}
	cm := makeRollbackCM(meta, envelopes)

	plan, err := BuildPlan(cm, nil)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	item := plan.Items[0]
	if item.Operation != "DELETE" {
		t.Errorf("expected operation DELETE, got %s", item.Operation)
	}
	if item.StoredRV != "" {
		t.Errorf("expected empty storedRV for Create, got %s", item.StoredRV)
	}
}

func TestBuildPlan_WithTxnItems(t *testing.T) {
	meta := rollback.Meta{
		Version:              2,
		TransactionName:      "txn-3",
		TransactionNamespace: "ns-1",
		Changes: []rollback.MetaChange{
			{
				Name:        "cm-a-change",
				Target:      rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-a", Namespace: "ns-1"},
				ChangeType:  "Patch",
				RollbackKey: "ConfigMap_ns-1_cm-a",
			},
			{
				Name:        "cm-b-change",
				Target:      rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-b", Namespace: "ns-1"},
				ChangeType:  "Update",
				RollbackKey: "ConfigMap_ns-1_cm-b",
			},
		},
	}
	envelopes := map[string]rollback.Envelope{
		"ConfigMap_ns-1_cm-a": {ResourceVersion: "10", ChangeType: "Patch", PriorState: json.RawMessage(`{}`)},
		"ConfigMap_ns-1_cm-b": {ResourceVersion: "20", ChangeType: "Update", PriorState: json.RawMessage(`{}`)},
	}
	cm := makeRollbackCM(meta, envelopes)

	txnItems := map[string]ItemStatusInfo{
		"cm-a-change": {Committed: true, RolledBack: true},  // already rolled back
		"cm-b-change": {Committed: true, RolledBack: false}, // committed, not rolled back
	}

	plan, err := BuildPlan(cm, txnItems)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if plan.Items[0].Status != StatusDone {
		t.Errorf("cm-a: expected done, got %s", plan.Items[0].Status)
	}
	if plan.Items[1].Status != StatusPending {
		t.Errorf("cm-b: expected pending, got %s", plan.Items[1].Status)
	}
}

func TestBuildPlan_UncommittedIsDone(t *testing.T) {
	meta := rollback.Meta{
		Version:              2,
		TransactionName:      "txn-4",
		TransactionNamespace: "ns-1",
		Changes: []rollback.MetaChange{
			{
				Name:        "cm-x-change",
				Target:      rollback.MetaTarget{APIVersion: "v1", Kind: "ConfigMap", Name: "cm-x", Namespace: "ns-1"},
				ChangeType:  "Patch",
				RollbackKey: "ConfigMap_ns-1_cm-x",
			},
		},
	}
	envelopes := map[string]rollback.Envelope{
		"ConfigMap_ns-1_cm-x": {ResourceVersion: "50", ChangeType: "Patch", PriorState: json.RawMessage(`{}`)},
	}
	cm := makeRollbackCM(meta, envelopes)

	// Item was never committed -- nothing to roll back.
	txnItems := map[string]ItemStatusInfo{
		"cm-x-change": {Committed: false, RolledBack: false},
	}

	plan, err := BuildPlan(cm, txnItems)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if plan.Items[0].Status != StatusDone {
		t.Errorf("expected done for uncommitted item, got %s", plan.Items[0].Status)
	}
}

func TestBuildPlan_MissingMeta(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-cm", Namespace: "ns-1"},
		Data:       map[string]string{"some-key": "value"},
	}
	_, err := BuildPlan(cm, nil)
	if err == nil {
		t.Fatal("expected error for missing _meta key, got nil")
	}
	if !strings.Contains(err.Error(), rollback.MetaKey) {
		t.Errorf("error should mention %s, got: %v", rollback.MetaKey, err)
	}
}

func TestFormatPlan(t *testing.T) {
	plan := &Plan{
		TransactionName:      "txn-fmt",
		TransactionNamespace: "ns-fmt",
		Items: []PlanItem{
			{Name: "cm-done-change", Target: rollback.MetaTarget{Kind: "ConfigMap", Namespace: "ns-fmt", Name: "cm-done"}, Operation: "RESTORE", Status: StatusDone},
			{Name: "cm-pending-change", Target: rollback.MetaTarget{Kind: "ConfigMap", Namespace: "ns-fmt", Name: "cm-pending"}, Operation: "DELETE", Status: StatusPending},
			{Name: "sec-conflict-change", Target: rollback.MetaTarget{Kind: "Secret", Namespace: "ns-fmt", Name: "sec-conflict"}, Operation: "RESTORE", Status: StatusConflict, Reason: "resource version mismatch"},
		},
	}
	out := FormatPlan(plan)

	for _, want := range []string{
		"txn-fmt",
		"ns-fmt",
		"[done]",
		"[pending]",
		"[conflict]",
		"RESTORE",
		"DELETE",
		"resource version mismatch",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatPlan output missing %q:\n%s", want, out)
		}
	}
}

func TestHasPending(t *testing.T) {
	allDone := &Plan{
		Items: []PlanItem{
			{Status: StatusDone},
			{Status: StatusDone},
		},
	}
	if allDone.HasPending() {
		t.Error("expected HasPending=false for all-done plan")
	}

	onePending := &Plan{
		Items: []PlanItem{
			{Status: StatusDone},
			{Status: StatusPending},
		},
	}
	if !onePending.HasPending() {
		t.Error("expected HasPending=true with one pending item")
	}

	oneConflict := &Plan{
		Items: []PlanItem{
			{Status: StatusDone},
			{Status: StatusConflict},
		},
	}
	if !oneConflict.HasPending() {
		t.Error("expected HasPending=true with one conflict item")
	}
}
