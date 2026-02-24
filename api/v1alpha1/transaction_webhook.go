package v1alpha1

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var _ admission.Validator[*Transaction] = &TransactionCustomValidator{}

// TransactionCustomValidator implements admission validation for Transaction.
type TransactionCustomValidator struct{}

// SetupWebhookWithManager registers the validating webhook with the manager.
func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &Transaction{}).
		WithValidator(&TransactionCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-backup-janus-io-v1alpha1-transaction,mutating=false,failurePolicy=fail,sideEffects=None,groups=tx.janus.io,resources=transactions,verbs=create;update,versions=v1alpha1,name=vtransaction-v1alpha1.kb.io,admissionReviewVersions=v1

func (v *TransactionCustomValidator) ValidateCreate(_ context.Context, txn *Transaction) (admission.Warnings, error) {
	return nil, validateTransactionSpec(txn, nil)
}

func (v *TransactionCustomValidator) ValidateUpdate(_ context.Context, oldTxn, newTxn *Transaction) (admission.Warnings, error) {
	return nil, validateTransactionSpec(newTxn, oldTxn)
}

func (v *TransactionCustomValidator) ValidateDelete(_ context.Context, _ *Transaction) (admission.Warnings, error) {
	return nil, nil
}

func validateTransactionSpec(txn *Transaction, old *Transaction) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if old != nil {
		// Cannot unseal.
		if old.Spec.Sealed && !txn.Spec.Sealed {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("sealed"),
				"cannot unseal a transaction"))
		}
		// Immutable once sealed (except for the seal itself).
		if old.Spec.Sealed && !specsEqual(old, txn) {
			allErrs = append(allErrs, field.Forbidden(specPath,
				"spec is immutable once sealed"))
		}
		// Immutable once processing begins.
		if old.Status.Phase != "" && old.Status.Phase != TransactionPhasePending {
			if !specsEqual(old, txn) {
				allErrs = append(allErrs, field.Forbidden(specPath,
					"spec is immutable once the transaction has left Pending phase"))
			}
		}
	}

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Transaction"},
			txn.Name, allErrs)
	}
	return nil
}

// specsEqual compares the specs of two Transactions for the immutability check.
func specsEqual(a, b *Transaction) bool {
	return reflect.DeepEqual(a.Spec, b.Spec)
}

// --- ResourceChange webhook ---

var _ admission.Validator[*ResourceChange] = &ResourceChangeCustomValidator{}

// ResourceChangeCustomValidator implements admission validation for ResourceChange.
type ResourceChangeCustomValidator struct{}

// SetupResourceChangeWebhookWithManager registers the ResourceChange validating webhook.
func SetupResourceChangeWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ResourceChange{}).
		WithValidator(&ResourceChangeCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-tx-janus-io-v1alpha1-resourcechange,mutating=false,failurePolicy=fail,sideEffects=None,groups=tx.janus.io,resources=resourcechanges,verbs=create;update,versions=v1alpha1,name=vresourcechange-v1alpha1.kb.io,admissionReviewVersions=v1

func (v *ResourceChangeCustomValidator) ValidateCreate(_ context.Context, obj *ResourceChange) (admission.Warnings, error) {
	return nil, validateResourceChangeSpec(obj)
}

func (v *ResourceChangeCustomValidator) ValidateUpdate(_ context.Context, _, _ *ResourceChange) (admission.Warnings, error) {
	// ResourceChange spec is always-immutable on update. Changes are created
	// before seal and never modified after.
	return nil, nil
}

func (v *ResourceChangeCustomValidator) ValidateDelete(_ context.Context, _ *ResourceChange) (admission.Warnings, error) {
	return nil, nil
}

func validateResourceChangeSpec(rc *ResourceChange) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	targetPath := specPath.Child("target")

	if rc.Spec.Target.APIVersion == "" {
		allErrs = append(allErrs, field.Required(targetPath.Child("apiVersion"), ""))
	}
	if rc.Spec.Target.Kind == "" {
		allErrs = append(allErrs, field.Required(targetPath.Child("kind"), ""))
	}
	if rc.Spec.Target.Name == "" {
		allErrs = append(allErrs, field.Required(targetPath.Child("name"), ""))
	}

	switch rc.Spec.Type {
	case ChangeTypeCreate, ChangeTypeUpdate, ChangeTypePatch:
		if len(rc.Spec.Content.Raw) == 0 {
			allErrs = append(allErrs, field.Required(specPath.Child("content"),
				fmt.Sprintf("content is required for %s", rc.Spec.Type)))
		}
	case ChangeTypeDelete:
		if len(rc.Spec.Content.Raw) > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("content"),
				"content must be empty for Delete"))
		}
	}

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "ResourceChange"},
			rc.Name, allErrs)
	}
	return nil
}
