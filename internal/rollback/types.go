// Package rollback defines the rollback ConfigMap contract shared between
// the controller and the recover CLI.
package rollback

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// MetaKey is the ConfigMap data key that holds transaction-level metadata.
const MetaKey = "_meta"

// Envelope wraps a single resource's rollback state in the ConfigMap.
type Envelope struct {
	// ResourceVersion of the resource at the time it was captured.
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// ChangeType that produced this envelope (Create, Patch, Delete, Update).
	ChangeType string `json:"changeType"`
	// PriorState is the raw JSON of the resource before the change.
	// Nil for Create (there was no prior state).
	PriorState json.RawMessage `json:"priorState,omitempty"`
}

// Meta is stored under MetaKey and describes the transaction as a whole.
type Meta struct {
	// Version of the rollback format. Currently 2.
	Version int `json:"version"`
	// TransactionName is the name of the owning Transaction.
	TransactionName string `json:"transactionName"`
	// TransactionNamespace is the namespace of the owning Transaction.
	TransactionNamespace string `json:"transactionNamespace"`
	// Changes lists each item in the transaction with its rollback key.
	Changes []MetaChange `json:"changes"`
}

// MetaChange records one transaction item's rollback location.
type MetaChange struct {
	// Name of the ResourceChange CR.
	Name string `json:"name"`
	// Target identifies the resource being changed.
	Target MetaTarget `json:"target"`
	// ChangeType that was applied (Create, Patch, Delete, Update).
	ChangeType string `json:"changeType"`
	// RollbackKey is the ConfigMap data key holding this item's Envelope.
	RollbackKey string `json:"rollbackKey"`
}

// MetaTarget identifies a Kubernetes resource.
// It mirrors api/v1alpha1.ResourceRef without importing it, so that the
// rollback package stays free of API dependencies.
type MetaTarget struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// Key returns the ConfigMap data key for a resource identified by kind,
// namespace, and name.
func Key(kind, namespace, name string) string {
	return fmt.Sprintf("%s_%s_%s", kind, namespace, name)
}

// CleanForRestore strips server-managed fields from an Unstructured object
// so it can be used for a restore (Update or Create). If targetNS is
// non-empty, the object's namespace is overridden.
func CleanForRestore(obj *unstructured.Unstructured, targetNS string) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetGeneration(0)
	obj.SetManagedFields(nil)
	delete(obj.Object, "status")
	if targetNS != "" {
		obj.SetNamespace(targetNS)
	}
}
