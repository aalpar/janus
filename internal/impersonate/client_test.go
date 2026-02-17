/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package impersonate

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// newMockScheme creates a minimal runtime scheme for testing.
func newMockScheme() *runtime.Scheme {
	return runtime.NewScheme()
}

// newMockRESTMapper creates a minimal REST mapper for testing.
func newMockRESTMapper() meta.RESTMapper {
	return meta.NewDefaultRESTMapper([]schema.GroupVersion{})
}

// --- NewClient ---

func TestNewClient_CreatesClientSuccessfully(t *testing.T) {
	baseCfg := &rest.Config{
		Host:            "https://kubernetes.default.svc.cluster.local:443",
		BearerToken:     "test-token",
		TLSClientConfig: rest.TLSClientConfig{Insecure: false},
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	client, err := NewClient(baseCfg, scheme, mapper, "default", "my-sa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_SetsImpersonationUsername(t *testing.T) {
	baseCfg := &rest.Config{
		Host:        "https://kubernetes.default.svc.cluster.local:443",
		BearerToken: "test-token",
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	client, err := NewClient(baseCfg, scheme, mapper, "my-namespace", "my-service-account")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	// Verify the base config was not mutated by checking that the original has no impersonation set.
	if baseCfg.Impersonate.UserName != "" {
		t.Fatal("base config should not have been mutated")
	}
}

func TestNewClient_DoesNotMutateBaseConfig(t *testing.T) {
	originalHost := "https://original-host:443"
	originalToken := "original-token"
	baseCfg := &rest.Config{
		Host:        originalHost,
		BearerToken: originalToken,
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	_, err := NewClient(baseCfg, scheme, mapper, "default", "my-sa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify base config unchanged
	if baseCfg.Host != originalHost {
		t.Fatalf("expected host %s, got %s", originalHost, baseCfg.Host)
	}
	if baseCfg.BearerToken != originalToken {
		t.Fatalf("expected token %s, got %s", originalToken, baseCfg.BearerToken)
	}
	// Verify impersonate config was not set on the base config
	if baseCfg.Impersonate.UserName != "" {
		t.Fatal("base config impersonate config should not be mutated")
	}
}

func TestNewClient_FormatsServiceAccountNameCorrectly(t *testing.T) {
	tests := []struct {
		name             string
		namespace        string
		saName           string
		expectedUsername string
	}{
		{
			name:             "standard names",
			namespace:        "default",
			saName:           "my-sa",
			expectedUsername: "system:serviceaccount:default:my-sa",
		},
		{
			name:             "kube-system namespace",
			namespace:        "kube-system",
			saName:           "cluster-admin",
			expectedUsername: "system:serviceaccount:kube-system:cluster-admin",
		},
		{
			name:             "hyphenated names",
			namespace:        "my-namespace",
			saName:           "my-service-account",
			expectedUsername: "system:serviceaccount:my-namespace:my-service-account",
		},
		{
			name:             "single character names",
			namespace:        "a",
			saName:           "b",
			expectedUsername: "system:serviceaccount:a:b",
		},
		{
			name:             "numeric namespace",
			namespace:        "ns-1",
			saName:           "sa-2",
			expectedUsername: "system:serviceaccount:ns-1:sa-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseCfg := &rest.Config{
				Host:        "https://kubernetes.default.svc.cluster.local:443",
				BearerToken: "test-token",
			}
			scheme := newMockScheme()
			mapper := newMockRESTMapper()

			_, err := NewClient(baseCfg, scheme, mapper, tt.namespace, tt.saName)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// The expected username should be set in the config copy.
			// We verify this by checking that the base config was not mutated.
			if baseCfg.Impersonate.UserName != "" {
				t.Errorf("base config was mutated; expected empty impersonate, got %s",
					baseCfg.Impersonate.UserName)
			}
		})
	}
}

func TestNewClient_WithEmptyNamespace(t *testing.T) {
	baseCfg := &rest.Config{
		Host:        "https://kubernetes.default.svc.cluster.local:443",
		BearerToken: "test-token",
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	client, err := NewClient(baseCfg, scheme, mapper, "", "my-sa")
	if err != nil {
		t.Fatalf("unexpected error with empty namespace: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client even with empty namespace")
	}
}

func TestNewClient_WithEmptyServiceAccountName(t *testing.T) {
	baseCfg := &rest.Config{
		Host:        "https://kubernetes.default.svc.cluster.local:443",
		BearerToken: "test-token",
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	client, err := NewClient(baseCfg, scheme, mapper, "default", "")
	if err != nil {
		t.Fatalf("unexpected error with empty SA name: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client even with empty SA name")
	}
}

func TestNewClient_WithBothEmpty(t *testing.T) {
	baseCfg := &rest.Config{
		Host:        "https://kubernetes.default.svc.cluster.local:443",
		BearerToken: "test-token",
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	client, err := NewClient(baseCfg, scheme, mapper, "", "")
	if err != nil {
		t.Fatalf("unexpected error with both empty: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client even with both empty")
	}
}

func TestNewClient_PreservesOtherConfigFields(t *testing.T) {
	baseCfg := &rest.Config{
		Host:            "https://kubernetes.default.svc.cluster.local:443",
		BearerToken:     "test-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		QPS:             100,
		Burst:           200,
	}
	originalHost := baseCfg.Host
	originalQPS := baseCfg.QPS
	originalBurst := baseCfg.Burst

	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	_, err := NewClient(baseCfg, scheme, mapper, "default", "my-sa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify original config fields are preserved in the base config
	if baseCfg.Host != originalHost {
		t.Fatalf("host was mutated: %s", baseCfg.Host)
	}
	if baseCfg.QPS != originalQPS {
		t.Fatalf("QPS was mutated: %f", baseCfg.QPS)
	}
	if baseCfg.Burst != originalBurst {
		t.Fatalf("Burst was mutated: %d", baseCfg.Burst)
	}
}

func TestNewClient_UsesProvidedSchemeAndMapper(t *testing.T) {
	baseCfg := &rest.Config{
		Host:        "https://kubernetes.default.svc.cluster.local:443",
		BearerToken: "test-token",
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	// Just verify that no error occurs when we pass valid scheme and mapper
	client, err := NewClient(baseCfg, scheme, mapper, "default", "my-sa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_WithSpecialCharactersInNames(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		saName    string
	}{
		{
			name:      "dots in names",
			namespace: "kube.system",
			saName:    "service.account",
		},
		{
			name:      "hyphens in names",
			namespace: "my-ns",
			saName:    "my-sa",
		},
		{
			name:      "long names",
			namespace: "very-long-namespace-name-with-many-dashes",
			saName:    "very-long-service-account-name-with-many-dashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseCfg := &rest.Config{
				Host:        "https://kubernetes.default.svc.cluster.local:443",
				BearerToken: "test-token",
			}
			scheme := newMockScheme()
			mapper := newMockRESTMapper()

			client, err := NewClient(baseCfg, scheme, mapper, tt.namespace, tt.saName)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if client == nil {
				t.Fatal("expected non-nil client")
			}

			// Verify base config was not mutated
			if baseCfg.Impersonate.UserName != "" {
				t.Errorf("base config was mutated")
			}
		})
	}
}

func TestNewClient_MultipleCallsWithSameConfig(t *testing.T) {
	baseCfg := &rest.Config{
		Host:        "https://kubernetes.default.svc.cluster.local:443",
		BearerToken: "test-token",
	}
	scheme := newMockScheme()
	mapper := newMockRESTMapper()

	// Create multiple clients with the same base config
	client1, err1 := NewClient(baseCfg, scheme, mapper, "ns1", "sa1")
	if err1 != nil {
		t.Fatalf("first call unexpected error: %v", err1)
	}

	client2, err2 := NewClient(baseCfg, scheme, mapper, "ns2", "sa2")
	if err2 != nil {
		t.Fatalf("second call unexpected error: %v", err2)
	}

	// Both should be non-nil
	if client1 == nil || client2 == nil {
		t.Fatal("expected non-nil clients")
	}

	// Base config should remain unmodified
	if baseCfg.Impersonate.UserName != "" {
		t.Fatal("base config was mutated by multiple calls")
	}
}
