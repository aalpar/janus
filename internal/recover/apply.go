package recover

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aalpar/janus/internal/rollback"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrConflict indicates a resource was modified since the rollback state was captured.
type ErrConflict struct {
	Target    rollback.MetaTarget
	StoredRV  string
	CurrentRV string
}

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("conflict on %s/%s/%s: stored RV %s, current RV %s",
		e.Target.Kind, e.Target.Namespace, e.Target.Name, e.StoredRV, e.CurrentRV)
}

// ApplyOptions controls the behavior of ApplyItem.
type ApplyOptions struct {
	Force bool // if true, apply even when RV conflict is detected
}

// ApplyItem executes a single rollback operation.
// It reads the envelope from the ConfigMap and applies the reverse operation.
// Returns nil on success, *ErrConflict on RV mismatch (unless Force),
// or another error on failure.
func ApplyItem(ctx context.Context, cl client.Client, item PlanItem, rbCM *corev1.ConfigMap, opts ApplyOptions) error {
	rbKey := rollback.Key(item.Target.Kind, item.Target.Namespace, item.Target.Name)
	raw, ok := rbCM.Data[rbKey]
	if !ok {
		return fmt.Errorf("no rollback data for key %s", rbKey)
	}
	var env rollback.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return fmt.Errorf("parsing envelope for %s: %w", rbKey, err)
	}

	switch item.Operation {
	case "DELETE":
		return applyDelete(ctx, cl, item.Target)
	case "RESTORE":
		return applyRestore(ctx, cl, item.Target, env, opts)
	default:
		return fmt.Errorf("unknown operation: %s", item.Operation)
	}
}

// applyDelete reverses a Create by deleting the resource.
// If the resource is already gone, this is a no-op.
func applyDelete(ctx context.Context, cl client.Client, target rollback.MetaTarget) error {
	obj, err := targetToUnstructured(target)
	if err != nil {
		return err
	}
	if err := cl.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone
		}
		return fmt.Errorf("deleting %s/%s/%s: %w", target.Kind, target.Namespace, target.Name, err)
	}
	return nil
}

// applyRestore reverses an Update, Patch, or Delete by restoring prior state.
func applyRestore(ctx context.Context, cl client.Client, target rollback.MetaTarget, env rollback.Envelope, opts ApplyOptions) error {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(env.PriorState, &obj.Object); err != nil {
		return fmt.Errorf("unmarshalling prior state for %s/%s/%s: %w", target.Kind, target.Namespace, target.Name, err)
	}

	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return fmt.Errorf("parsing apiVersion %q: %w", target.APIVersion, err)
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gv.Group, Version: gv.Version, Kind: target.Kind,
	})
	if target.Namespace != "" {
		obj.SetNamespace(target.Namespace)
	}

	// Strip server-managed fields before applying (mirrors controller cleanForRestore).
	cleanForRestore(obj)

	// Fetch current state to decide between create and update.
	existing, err := targetToUnstructured(target)
	if err != nil {
		return err
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		if apierrors.IsNotFound(err) {
			// Resource was deleted externally; recreate from prior state.
			return cl.Create(ctx, obj)
		}
		return fmt.Errorf("fetching %s/%s/%s: %w", target.Kind, target.Namespace, target.Name, err)
	}

	// RV conflict check: if the envelope recorded a RV, compare it to current.
	if env.ResourceVersion != "" && existing.GetResourceVersion() != env.ResourceVersion {
		if !opts.Force {
			return &ErrConflict{
				Target:    target,
				StoredRV:  env.ResourceVersion,
				CurrentRV: existing.GetResourceVersion(),
			}
		}
	}

	// Update with the existing object's RV so the API server accepts it.
	obj.SetResourceVersion(existing.GetResourceVersion())
	return cl.Update(ctx, obj)
}

// targetToUnstructured builds a minimal Unstructured for Get/Delete from a MetaTarget.
func targetToUnstructured(target rollback.MetaTarget) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing apiVersion %q: %w", target.APIVersion, err)
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gv.Group, Version: gv.Version, Kind: target.Kind,
	})
	obj.SetName(target.Name)
	obj.SetNamespace(target.Namespace)
	return obj, nil
}

// cleanForRestore strips server-managed fields from a resource before applying.
// This mirrors the controller's cleanForRestore logic.
func cleanForRestore(obj *unstructured.Unstructured) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetGeneration(0)
	obj.SetManagedFields(nil)
	delete(obj.Object, "status")
}
