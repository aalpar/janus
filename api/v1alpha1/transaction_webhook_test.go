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
		},
	}
}

// --- Transaction webhook tests ---

func TestValidateCreate_ValidSpec(t *testing.T) {
	v := &TransactionCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), validTxn())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateUpdate_SealTransaction(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	newTxn := validTxn()
	newTxn.Spec.Sealed = true
	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err != nil {
		t.Fatalf("expected no error for sealing, got %v", err)
	}
}

func TestValidateUpdate_CannotUnseal(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Spec.Sealed = true
	newTxn := validTxn()
	newTxn.Spec.Sealed = false
	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err == nil {
		t.Fatal("expected error for unsealing")
	}
}

func TestValidateUpdate_ImmutableOnceSealed(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Spec.Sealed = true
	newTxn := validTxn()
	newTxn.Spec.Sealed = true
	newTxn.Spec.ServiceAccountName = "different-sa" //nolint:goconst // test data
	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err == nil {
		t.Fatal("expected error for spec change after sealing")
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

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	v := &TransactionCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), validTxn())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

// --- ResourceChange webhook tests ---

func validResourceChange() *ResourceChange {
	return &ResourceChange{
		ObjectMeta: metav1.ObjectMeta{Name: "test-rc", Namespace: "default"},
		Spec: ResourceChangeSpec{
			Target: ResourceRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       "my-cm",
			},
			Type:    ChangeTypeCreate,
			Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
		},
	}
}

func TestResourceChange_ValidCreate(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), validResourceChange())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestResourceChange_MissingContent(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	rc := validResourceChange()
	rc.Spec.Content = runtime.RawExtension{}
	_, err := v.ValidateCreate(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error for missing content on Create")
	}
}

func TestResourceChange_DeleteWithContent(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	rc := validResourceChange()
	rc.Spec.Type = ChangeTypeDelete
	_, err := v.ValidateCreate(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error for Delete with content")
	}
}

func TestResourceChange_DeleteWithoutContent(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	rc := validResourceChange()
	rc.Spec.Type = ChangeTypeDelete
	rc.Spec.Content = runtime.RawExtension{}
	_, err := v.ValidateCreate(context.Background(), rc)
	if err != nil {
		t.Fatalf("expected no error for Delete without content, got %v", err)
	}
}

func TestResourceChange_UpdateAlwaysAllowed(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	_, err := v.ValidateUpdate(context.Background(), validResourceChange(), validResourceChange())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestResourceChange_DeleteAlwaysAllowed(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), validResourceChange())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestResourceChange_EmptyTargetFields(t *testing.T) {
	v := &ResourceChangeCustomValidator{}
	rc := validResourceChange()
	rc.Spec.Target = ResourceRef{}
	_, err := v.ValidateCreate(context.Background(), rc)
	if err == nil {
		t.Fatal("expected error for empty target fields")
	}
}
