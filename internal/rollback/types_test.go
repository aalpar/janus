package rollback

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEnvelopeMarshalRoundTrip(t *testing.T) {
	orig := Envelope{
		ResourceVersion: "12345",
		ChangeType:      "Patch",
		PriorState:      json.RawMessage(`{"apiVersion":"v1","kind":"ConfigMap"}`),
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.ResourceVersion != orig.ResourceVersion {
		t.Errorf("ResourceVersion = %q, want %q", got.ResourceVersion, orig.ResourceVersion)
	}
	if got.ChangeType != orig.ChangeType {
		t.Errorf("ChangeType = %q, want %q", got.ChangeType, orig.ChangeType)
	}
	if string(got.PriorState) != string(orig.PriorState) {
		t.Errorf("PriorState = %s, want %s", got.PriorState, orig.PriorState)
	}
}

func TestEnvelopeCreateHasNilPriorState(t *testing.T) {
	orig := Envelope{
		ChangeType: "Create",
		// PriorState intentionally nil — Create has no prior state.
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.PriorState != nil {
		t.Errorf("PriorState = %s, want nil", got.PriorState)
	}
	if got.ChangeType != "Create" {
		t.Errorf("ChangeType = %q, want %q", got.ChangeType, "Create")
	}
}

func TestMetaMarshalRoundTrip(t *testing.T) {
	orig := Meta{
		Version:              2,
		TransactionName:      "restore-backup-1",
		TransactionNamespace: "default",
		Changes: []MetaChange{
			{
				Name: "my-cm-change",
				Target: MetaTarget{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "my-cm",
					Namespace:  "default",
				},
				ChangeType:  "Create",
				RollbackKey: "ConfigMap_default_my-cm",
			},
			{
				Name: "my-deploy-change",
				Target: MetaTarget{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "my-deploy",
					Namespace:  "default",
				},
				ChangeType:  "Patch",
				RollbackKey: "Deployment_default_my-deploy",
			},
		},
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Meta
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Version != orig.Version {
		t.Errorf("Version = %d, want %d", got.Version, orig.Version)
	}
	if got.TransactionName != orig.TransactionName {
		t.Errorf("TransactionName = %q, want %q", got.TransactionName, orig.TransactionName)
	}
	if got.TransactionNamespace != orig.TransactionNamespace {
		t.Errorf("TransactionNamespace = %q, want %q", got.TransactionNamespace, orig.TransactionNamespace)
	}
	if len(got.Changes) != len(orig.Changes) {
		t.Fatalf("len(Changes) = %d, want %d", len(got.Changes), len(orig.Changes))
	}
	for i, wantC := range orig.Changes {
		gotC := got.Changes[i]
		if gotC.Name != wantC.Name {
			t.Errorf("Changes[%d].Name = %q, want %q", i, gotC.Name, wantC.Name)
		}
		if gotC.Target != wantC.Target {
			t.Errorf("Changes[%d].Target = %+v, want %+v", i, gotC.Target, wantC.Target)
		}
		if gotC.ChangeType != wantC.ChangeType {
			t.Errorf("Changes[%d].ChangeType = %q, want %q", i, gotC.ChangeType, wantC.ChangeType)
		}
		if gotC.RollbackKey != wantC.RollbackKey {
			t.Errorf("Changes[%d].RollbackKey = %q, want %q", i, gotC.RollbackKey, wantC.RollbackKey)
		}
	}
}

func TestCleanForRestore(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":              "my-cm",
				"namespace":         "original-ns",
				"resourceVersion":   "999",
				"uid":               "abc-123",
				"creationTimestamp":  "2024-01-01T00:00:00Z",
				"generation":        int64(3),
				"managedFields":     []interface{}{map[string]interface{}{"manager": "kubectl"}},
			},
			"status": map[string]interface{}{"phase": "Active"},
			"data":   map[string]interface{}{"key": "value"},
		},
	}

	CleanForRestore(obj, "target-ns")

	if rv := obj.GetResourceVersion(); rv != "" {
		t.Errorf("ResourceVersion = %q, want empty", rv)
	}
	if uid := string(obj.GetUID()); uid != "" {
		t.Errorf("UID = %q, want empty", uid)
	}
	if ct := obj.GetCreationTimestamp(); ct != (metav1.Time{}) {
		t.Errorf("CreationTimestamp = %v, want zero", ct)
	}
	if gen := obj.GetGeneration(); gen != 0 {
		t.Errorf("Generation = %d, want 0", gen)
	}
	if mf := obj.GetManagedFields(); mf != nil {
		t.Errorf("ManagedFields = %v, want nil", mf)
	}
	if _, ok := obj.Object["status"]; ok {
		t.Error("status field still present, want deleted")
	}
	if ns := obj.GetNamespace(); ns != "target-ns" {
		t.Errorf("Namespace = %q, want %q", ns, "target-ns")
	}
	// Verify data is preserved.
	data, _, _ := unstructured.NestedString(obj.Object, "data", "key")
	if data != "value" {
		t.Errorf("data.key = %q, want %q", data, "value")
	}
}

func TestCleanForRestoreEmptyTargetNS(t *testing.T) {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]interface{}{
				"name":            "my-cm",
				"namespace":       "original-ns",
				"resourceVersion": "1",
			},
		},
	}

	CleanForRestore(obj, "")

	if ns := obj.GetNamespace(); ns != "original-ns" {
		t.Errorf("Namespace = %q, want %q (preserved when targetNS empty)", ns, "original-ns")
	}
}

func TestKey(t *testing.T) {
	got := Key("ConfigMap", "default", "my-cm")
	want := "ConfigMap_default_my-cm"
	if got != want {
		t.Errorf("Key(ConfigMap, default, my-cm) = %q, want %q", got, want)
	}
}
