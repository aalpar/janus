package rollback

import (
	"encoding/json"
	"testing"
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
		Version:              1,
		TransactionName:      "restore-backup-1",
		TransactionNamespace: "default",
		Changes: []MetaChange{
			{
				Index: 0,
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
				Index: 1,
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
		if gotC.Index != wantC.Index {
			t.Errorf("Changes[%d].Index = %d, want %d", i, gotC.Index, wantC.Index)
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

func TestKey(t *testing.T) {
	got := Key("ConfigMap", "default", "my-cm")
	want := "ConfigMap_default_my-cm"
	if got != want {
		t.Errorf("Key(ConfigMap, default, my-cm) = %q, want %q", got, want)
	}
}
