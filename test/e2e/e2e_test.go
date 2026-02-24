//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aalpar/janus/test/utils"
)

// namespace where the project is deployed in
const namespace = "janus-system"

// serviceAccountName created for the project
const serviceAccountName = "janus-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "janus-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "janus-metrics-binding"

// testNS is the namespace where Transaction e2e test resources are created.
const testNS = "janus-e2e-test"

// testSA is the ServiceAccount that Transactions impersonate during e2e tests.
const testSA = "txn-e2e-sa"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		_, err := kubectl("create", "ns", namespace)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		_, err = kubectl("label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd := exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		kubectlDeleteIgnoreNotFound("pod", "curl-metrics", namespace)

		By("removing metrics ClusterRoleBinding")
		_, _ = kubectl("delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")

		By("undeploying the controller-manager")
		cmd := exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		_, _ = kubectl("delete", "ns", namespace)
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		debugSources := []struct {
			label string
			args  []string
		}{
			{"Controller logs", []string{"logs", controllerPodName, "-n", namespace}},
			{"Kubernetes events", []string{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"}},
			{"Metrics logs", []string{"logs", "curl-metrics", "-n", namespace}},
			{"Controller pod description", []string{"describe", "pod", controllerPodName, "-n", namespace}},
		}
		for _, d := range debugSources {
			By("Fetching " + d.label)
			output, err := kubectl(d.args...)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "%s:\n%s", d.label, output)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get %s: %s", d.label, err)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				podOutput, err := kubectl("get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				output, err := kubectlGetField("pods", controllerPodName, namespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			_, err := kubectl("create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=janus-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			_, err = kubectl("get", "service", metricsServiceName, "-n", namespace)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			Eventually(func(g Gomega) {
				output, err := kubectlGetField("pod", controllerPodName, namespace,
					"{.status.conditions[?(@.type=='Ready')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			Eventually(func(g Gomega) {
				output, err := kubectl("logs", controllerPodName, "-n", namespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			_, err = kubectl("run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			Eventually(func(g Gomega) {
				output, err := kubectlGetField("pods", "curl-metrics", namespace, "{.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			Eventually(func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("Transaction lifecycle", func() {
		BeforeAll(func() {
			By("creating the e2e test namespace")
			_, err := kubectl("create", "ns", testNS)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")

			By("creating the test ServiceAccount")
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: %s
`, testSA, testNS))).To(Succeed())

			By("creating RBAC for the test ServiceAccount")
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: txn-e2e-role
  namespace: %s
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get","list","watch","create","update","patch","delete"]
`, testNS))).To(Succeed())

			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: txn-e2e-binding
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: txn-e2e-role
subjects:
- kind: ServiceAccount
  name: %s
  namespace: %s
`, testNS, testSA, testNS))).To(Succeed())
		})

		AfterAll(func() {
			By("deleting the e2e test namespace")
			_, _ = kubectl("delete", "ns", testNS, "--ignore-not-found")
		})

		It("should create a ConfigMap via Transaction", func() {
			txnName := "e2e-create"
			Expect(kubectlApplyInput(transactionYAML(txnName, txnChange{
				Kind: "ConfigMap", Name: "created-cm", ChangeType: "Create",
				Content: configMapYAML("created-cm", "key1: val1"),
			}))).To(Succeed())
			DeferCleanup(kubectlDeleteIgnoreNotFound, "transaction", txnName, testNS)

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying the ConfigMap exists with expected data")
			val, err := kubectlGetField("configmap", "created-cm", testNS, "{.data.key1}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("val1"))
		})

		It("should patch an existing ConfigMap via Transaction", func() {
			By("creating the prerequisite ConfigMap")
			applyConfigMap("patch-target", "existing: preserved")
			DeferCleanup(kubectlDeleteIgnoreNotFound, "configmap", "patch-target", testNS)

			txnName := "e2e-patch"
			Expect(kubectlApplyInput(transactionYAML(txnName, txnChange{
				Kind: "ConfigMap", Name: "patch-target", ChangeType: "Patch",
				Content: "data:\n  added: new-value",
			}))).To(Succeed())
			DeferCleanup(kubectlDeleteIgnoreNotFound, "transaction", txnName, testNS)

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying the patch was merged")
			val, err := kubectlGetField("configmap", "patch-target", testNS, "{.data.added}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("new-value"))

			By("verifying existing data was preserved")
			val, err = kubectlGetField("configmap", "patch-target", testNS, "{.data.existing}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("preserved"))
		})

		It("should delete an existing ConfigMap via Transaction", func() {
			By("creating the prerequisite ConfigMap")
			applyConfigMap("delete-target", `doomed: "true"`)

			txnName := "e2e-delete"
			Expect(kubectlApplyInput(transactionYAML(txnName, txnChange{
				Kind: "ConfigMap", Name: "delete-target", ChangeType: "Delete",
			}))).To(Succeed())
			DeferCleanup(kubectlDeleteIgnoreNotFound, "transaction", txnName, testNS)

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying the ConfigMap is gone")
			_, err := kubectl("get", "configmap", "delete-target", "-n", testNS)
			Expect(err).To(HaveOccurred(), "ConfigMap should have been deleted")
		})

		It("should handle a multi-item transaction (create + patch + delete)", func() {
			By("creating prerequisite ConfigMaps")
			applyConfigMap("multi-patch-target", "original: kept")
			applyConfigMap("multi-delete-target", `ephemeral: "true"`)

			txnName := "e2e-multi"
			Expect(kubectlApplyInput(transactionYAML(txnName,
				txnChange{
					Kind: "ConfigMap", Name: "multi-create-cm", ChangeType: "Create",
					Content: configMapYAML("multi-create-cm", `created: "true"`),
				},
				txnChange{
					Kind: "ConfigMap", Name: "multi-patch-target", ChangeType: "Patch",
					Content: "data:\n  patched: \"true\"",
				},
				txnChange{
					Kind: "ConfigMap", Name: "multi-delete-target", ChangeType: "Delete",
				},
			))).To(Succeed())
			DeferCleanup(kubectlDeleteIgnoreNotFound, "transaction", txnName, testNS)
			DeferCleanup(kubectlDeleteIgnoreNotFound, "configmap", "multi-create-cm", testNS)
			DeferCleanup(kubectlDeleteIgnoreNotFound, "configmap", "multi-patch-target", testNS)

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying created ConfigMap exists")
			val, err := kubectlGetField("configmap", "multi-create-cm", testNS, "{.data.created}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("true"))

			By("verifying patched ConfigMap has new data")
			val, err = kubectlGetField("configmap", "multi-patch-target", testNS, "{.data.patched}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("true"))

			By("verifying deleted ConfigMap is gone")
			_, err = kubectl("get", "configmap", "multi-delete-target", "-n", testNS)
			Expect(err).To(HaveOccurred(), "ConfigMap should have been deleted")
		})

		It("should rollback committed changes when a later item fails", func() {
			By("creating the prerequisite ConfigMap for patch rollback")
			applyConfigMap("rollback-patch-target", "original: kept")
			DeferCleanup(kubectlDeleteIgnoreNotFound, "configmap", "rollback-patch-target", testNS)

			txnName := "e2e-rollback"
			Expect(kubectlApplyInput(transactionYAML(txnName,
				txnChange{
					Kind: "ConfigMap", Name: "rollback-patch-target", ChangeType: "Patch",
					Content: "data:\n  patched: \"yes\"",
				},
				txnChange{
					Kind: "ConfigMap", Name: "rollback-created-cm", ChangeType: "Create",
					Content: configMapYAML("rollback-created-cm", `ephemeral: "true"`),
				},
				txnChange{
					Kind: "Secret", Name: "forbidden-secret", ChangeType: "Create",
					Content: fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: forbidden-secret
  namespace: %s
stringData:
  secret-key: secret-val`, testNS),
				},
			))).To(Succeed())
			DeferCleanup(kubectlDeleteIgnoreNotFound, "transaction", txnName, testNS)

			waitForTransactionPhase(txnName, testNS, "RolledBack")

			By("verifying the patched ConfigMap was reverted to its original state")
			val, err := kubectlGetField("configmap", "rollback-patch-target", testNS, "{.data.original}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("kept"))

			val, err = kubectlGetField("configmap", "rollback-patch-target", testNS, "{.data.patched}")
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(BeEmpty(), "patched key should have been reverted")

			By("verifying the created ConfigMap was deleted by rollback")
			_, err = kubectl("get", "configmap", "rollback-created-cm", "-n", testNS)
			Expect(err).To(HaveOccurred(), "rollback-created-cm should not exist after rollback")

			By("verifying the forbidden Secret was never created")
			_, err = kubectl("get", "secret", "forbidden-secret", "-n", testNS)
			Expect(err).To(HaveOccurred(), "forbidden-secret should not exist")
		})

		It("should fail when referencing a non-existent ServiceAccount", func() {
			txnName := "e2e-bad-sa"
			Expect(kubectlApplyInput(transactionYAMLWithSA(txnName, "does-not-exist", txnChange{
				Kind: "ConfigMap", Name: "irrelevant", ChangeType: "Create",
				Content: configMapYAML("irrelevant", "key: val"),
			}))).To(Succeed())
			DeferCleanup(kubectlDeleteIgnoreNotFound, "transaction", txnName, testNS)

			waitForTransactionPhase(txnName, testNS, "Failed")

			By("verifying the Failed condition is present")
			val, err := kubectlGetField("transaction", txnName, testNS,
				`{.status.conditions[?(@.type=="Failed")].status}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal("True"))
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		output, err := kubectl("create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)
		g.Expect(err).NotTo(HaveOccurred())

		var token tokenRequest
		g.Expect(json.Unmarshal([]byte(output), &token)).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	return kubectl("logs", "curl-metrics", "-n", namespace)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// kubectl runs a kubectl command and returns its output.
func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

// kubectlGetField retrieves a single field from a namespaced resource via jsonpath.
func kubectlGetField(resource, name, ns, jsonpath string) (string, error) {
	return kubectl("get", resource, name, "-n", ns, "-o", fmt.Sprintf("jsonpath=%s", jsonpath))
}

// kubectlApplyInput pipes the given YAML to kubectl apply -f -.
func kubectlApplyInput(yaml string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	_, err := utils.Run(cmd)
	return err
}

// kubectlDeleteIgnoreNotFound deletes the named resource, ignoring NotFound errors.
func kubectlDeleteIgnoreNotFound(resource, name, ns string) {
	_, _ = kubectl("delete", resource, name, "-n", ns, "--ignore-not-found")
}

// waitForTransactionPhase polls until the Transaction reaches the expected phase.
func waitForTransactionPhase(name, ns, phase string) {
	By(fmt.Sprintf("waiting for Transaction %s/%s to reach phase %s", ns, name, phase))
	Eventually(func(g Gomega) {
		output, err := kubectlGetField("transaction", name, ns, "{.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal(phase))
	}, 2*time.Minute, time.Second).Should(Succeed())
}

// txnChange describes a single change item in a Transaction.
type txnChange struct {
	Kind       string // e.g. "ConfigMap", "Secret"
	Name       string
	ChangeType string // "Create", "Patch", "Delete"
	Content    string // raw YAML indented under content:; empty for Delete
}

// transactionYAML builds Transaction YAML in testNS using testSA.
func transactionYAML(name string, changes ...txnChange) string {
	return transactionYAMLWithSA(name, testSA, changes...)
}

// transactionYAMLWithSA builds Transaction YAML with a custom ServiceAccount.
func transactionYAMLWithSA(name, sa string, changes ...txnChange) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: tx.janus.io/v1alpha1
kind: Transaction
metadata:
  name: %s
  namespace: %s
spec:
  serviceAccountName: %s
  changes:
`, name, testNS, sa)
	for _, c := range changes {
		fmt.Fprintf(&b, `  - target:
      apiVersion: v1
      kind: %s
      name: %s
      namespace: %s
    type: %s
`, c.Kind, c.Name, testNS, c.ChangeType)
		if c.Content != "" {
			b.WriteString("    content:\n")
			for _, line := range strings.Split(c.Content, "\n") {
				if line == "" {
					b.WriteString("\n")
				} else {
					fmt.Fprintf(&b, "      %s\n", line)
				}
			}
		}
	}
	return b.String()
}

// configMapYAML returns full ConfigMap YAML for testNS.
func configMapYAML(name, data string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  %s`, name, testNS, data)
}

// applyConfigMap creates a ConfigMap in testNS.
func applyConfigMap(name, data string) {
	Expect(kubectlApplyInput(configMapYAML(name, data))).To(Succeed())
}
