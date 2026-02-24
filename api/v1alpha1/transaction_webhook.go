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

	// Immutability: reject spec changes after processing begins.
	if old != nil && old.Status.Phase != "" && old.Status.Phase != TransactionPhasePending {
		if !specsEqual(old, txn) {
			allErrs = append(allErrs, field.Forbidden(specPath,
				"spec is immutable once the transaction has left Pending phase"))
			return apierrors.NewInvalid(
				schema.GroupKind{Group: GroupVersion.Group, Kind: "Transaction"},
				txn.Name, allErrs)
		}
	}

	changesPath := specPath.Child("changes")
	seen := make(map[string]int) // target key -> index (for duplicate detection)

	for i, change := range txn.Spec.Changes {
		changePath := changesPath.Index(i)
		targetPath := changePath.Child("target")

		// Target fields non-empty.
		if change.Target.APIVersion == "" {
			allErrs = append(allErrs, field.Required(targetPath.Child("apiVersion"), ""))
		}
		if change.Target.Kind == "" {
			allErrs = append(allErrs, field.Required(targetPath.Child("kind"), ""))
		}
		if change.Target.Name == "" {
			allErrs = append(allErrs, field.Required(targetPath.Child("name"), ""))
		}

		// Content required for Create/Update/Patch, forbidden for Delete.
		switch change.Type {
		case ChangeTypeCreate, ChangeTypeUpdate, ChangeTypePatch:
			if len(change.Content.Raw) == 0 {
				allErrs = append(allErrs, field.Required(changePath.Child("content"),
					fmt.Sprintf("content is required for %s", change.Type)))
			}
		case ChangeTypeDelete:
			if len(change.Content.Raw) > 0 {
				allErrs = append(allErrs, field.Forbidden(changePath.Child("content"),
					"content must be empty for Delete"))
			}
		}

		// Duplicate target detection.
		ns := change.Target.Namespace
		if ns == "" {
			ns = txn.Namespace
		}
		key := fmt.Sprintf("%s/%s/%s/%s", change.Target.APIVersion, change.Target.Kind, ns, change.Target.Name)
		if prev, exists := seen[key]; exists {
			allErrs = append(allErrs, field.Duplicate(targetPath,
				fmt.Sprintf("same target as changes[%d]", prev)))
		}
		seen[key] = i
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
