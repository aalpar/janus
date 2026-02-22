package v1alpha1

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func validTxn() *Transaction {
	return &Transaction{
		ObjectMeta: metav1.ObjectMeta{Name: "test-txn", Namespace: "default"},
		Spec: TransactionSpec{
			ServiceAccountName: "test-sa",
			Changes: []ResourceChange{{
				Target: ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "my-cm",
				},
				Type:    ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
			}},
		},
	}
}

func TestValidateCreate_ValidSpec(t *testing.T) {
	v := &TransactionCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), validTxn())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateCreate_MissingContent(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Content = runtime.RawExtension{}
	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for missing content on Create")
	}
}

func TestValidateCreate_DeleteWithContent(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Type = ChangeTypeDelete
	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for Delete with content")
	}
}

func TestValidateCreate_DeleteWithoutContent(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Type = ChangeTypeDelete
	txn.Spec.Changes[0].Content = runtime.RawExtension{}
	_, err := v.ValidateCreate(context.Background(), txn)
	if err != nil {
		t.Fatalf("expected no error for Delete without content, got %v", err)
	}
}

func TestValidateCreate_EmptyTargetFields(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Target = ResourceRef{}
	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for empty target fields")
	}
}

func TestValidateCreate_DuplicateTargets(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes = append(txn.Spec.Changes, txn.Spec.Changes[0])
	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for duplicate targets")
	}
}

func TestValidateUpdate_ImmutableAfterProcessing(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePreparing
	newTxn := validTxn()
	newTxn.Spec.ServiceAccountName = "different-sa"
	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err == nil {
		t.Fatal("expected error for spec change after Pending phase")
	}
}

func TestValidateUpdate_AllowSpecChangeInPending(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePending
	newTxn := validTxn()
	newTxn.Spec.ServiceAccountName = "different-sa"
	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err != nil {
		t.Fatalf("expected no error for spec change in Pending, got %v", err)
	}
}

func TestValidateUpdate_ImmutableTimeoutChange(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePreparing
	oldTxn.Spec.Timeout = &metav1.Duration{Duration: 5 * time.Minute}

	newTxn := validTxn()
	newTxn.Spec.Timeout = &metav1.Duration{Duration: 10 * time.Minute}

	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err == nil {
		t.Fatal("expected error for timeout change after Pending phase")
	}
}

func TestValidateUpdate_AllowStatusOnlyChange(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePreparing
	newTxn := validTxn()
	newTxn.Status.Phase = TransactionPhasePrepared
	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err != nil {
		t.Fatalf("expected no error for status-only change, got %v", err)
	}
}
