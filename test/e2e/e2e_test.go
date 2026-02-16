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
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
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
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=janus-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
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
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("Transaction lifecycle", func() {
		BeforeAll(func() {
			By("creating the e2e test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNS)
			_, err := utils.Run(cmd)
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
			cmd := exec.Command("kubectl", "delete", "ns", testNS, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should create a ConfigMap via Transaction", func() {
			txnName := "e2e-create"
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: %s
  namespace: %s
spec:
  serviceAccountName: %s
  changes:
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: created-cm
      namespace: %s
    type: Create
    content:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: created-cm
        namespace: %s
      data:
        key1: val1
`, txnName, testNS, testSA, testNS, testNS))).To(Succeed())

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying the ConfigMap exists with expected data")
			cmd := exec.Command("kubectl", "get", "configmap", "created-cm",
				"-n", testNS, "-o", "jsonpath={.data.key1}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("val1"))

			kubectlDeleteIgnoreNotFound("transaction", txnName, testNS)
		})

		It("should patch an existing ConfigMap via Transaction", func() {
			By("creating the prerequisite ConfigMap")
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: patch-target
  namespace: %s
data:
  existing: preserved
`, testNS))).To(Succeed())

			txnName := "e2e-patch"
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: %s
  namespace: %s
spec:
  serviceAccountName: %s
  changes:
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: patch-target
      namespace: %s
    type: Patch
    content:
      data:
        added: new-value
`, txnName, testNS, testSA, testNS))).To(Succeed())

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying the patch was merged")
			cmd := exec.Command("kubectl", "get", "configmap", "patch-target",
				"-n", testNS, "-o", "jsonpath={.data.added}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("new-value"))

			By("verifying existing data was preserved")
			cmd = exec.Command("kubectl", "get", "configmap", "patch-target",
				"-n", testNS, "-o", "jsonpath={.data.existing}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("preserved"))

			kubectlDeleteIgnoreNotFound("transaction", txnName, testNS)
			kubectlDeleteIgnoreNotFound("configmap", "patch-target", testNS)
		})

		It("should delete an existing ConfigMap via Transaction", func() {
			By("creating the prerequisite ConfigMap")
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: delete-target
  namespace: %s
data:
  doomed: "true"
`, testNS))).To(Succeed())

			txnName := "e2e-delete"
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: %s
  namespace: %s
spec:
  serviceAccountName: %s
  changes:
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: delete-target
      namespace: %s
    type: Delete
`, txnName, testNS, testSA, testNS))).To(Succeed())

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying the ConfigMap is gone")
			cmd := exec.Command("kubectl", "get", "configmap", "delete-target", "-n", testNS)
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "ConfigMap should have been deleted")

			kubectlDeleteIgnoreNotFound("transaction", txnName, testNS)
		})

		It("should handle a multi-item transaction (create + patch + delete)", func() {
			By("creating prerequisite ConfigMaps")
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: multi-patch-target
  namespace: %s
data:
  original: kept
`, testNS))).To(Succeed())
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: multi-delete-target
  namespace: %s
data:
  ephemeral: "true"
`, testNS))).To(Succeed())

			txnName := "e2e-multi"
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: %s
  namespace: %s
spec:
  serviceAccountName: %s
  changes:
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: multi-create-cm
      namespace: %s
    type: Create
    content:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: multi-create-cm
        namespace: %s
      data:
        created: "true"
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: multi-patch-target
      namespace: %s
    type: Patch
    content:
      data:
        patched: "true"
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: multi-delete-target
      namespace: %s
    type: Delete
`, txnName, testNS, testSA, testNS, testNS, testNS, testNS))).To(Succeed())

			waitForTransactionPhase(txnName, testNS, "Committed")

			By("verifying created ConfigMap exists")
			cmd := exec.Command("kubectl", "get", "configmap", "multi-create-cm",
				"-n", testNS, "-o", "jsonpath={.data.created}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"))

			By("verifying patched ConfigMap has new data")
			cmd = exec.Command("kubectl", "get", "configmap", "multi-patch-target",
				"-n", testNS, "-o", "jsonpath={.data.patched}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("true"))

			By("verifying deleted ConfigMap is gone")
			cmd = exec.Command("kubectl", "get", "configmap", "multi-delete-target", "-n", testNS)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "ConfigMap should have been deleted")

			kubectlDeleteIgnoreNotFound("transaction", txnName, testNS)
			kubectlDeleteIgnoreNotFound("configmap", "multi-create-cm", testNS)
			kubectlDeleteIgnoreNotFound("configmap", "multi-patch-target", testNS)
		})

		It("should fail when referencing a non-existent ServiceAccount", func() {
			txnName := "e2e-bad-sa"
			Expect(kubectlApplyInput(fmt.Sprintf(`apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: %s
  namespace: %s
spec:
  serviceAccountName: does-not-exist
  changes:
  - target:
      apiVersion: v1
      kind: ConfigMap
      name: irrelevant
      namespace: %s
    type: Create
    content:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: irrelevant
        namespace: %s
      data:
        key: val
`, txnName, testNS, testNS, testNS))).To(Succeed())

			waitForTransactionPhase(txnName, testNS, "Failed")

			By("verifying the Failed condition is present")
			cmd := exec.Command("kubectl", "get", "transaction", txnName,
				"-n", testNS, "-o",
				`jsonpath={.status.conditions[?(@.type=="Failed")].status}`)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("True"))

			kubectlDeleteIgnoreNotFound("transaction", txnName, testNS)
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
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
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
	cmd := exec.Command("kubectl", "delete", resource, name, "-n", ns, "--ignore-not-found")
	_, _ = utils.Run(cmd)
}

// waitForTransactionPhase polls until the Transaction reaches the expected phase or the timeout fires.
func waitForTransactionPhase(name, ns, phase string) {
	By(fmt.Sprintf("waiting for Transaction %s/%s to reach phase %s", ns, name, phase))
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "transaction", name,
			"-n", ns, "-o", "jsonpath={.status.phase}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal(phase))
	}, 2*time.Minute, time.Second).Should(Succeed())
}
