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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	flag "github.com/spf13/pflag"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/recover"
	"github.com/aalpar/janus/internal/scheme"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	sigsyaml "sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "create":
			os.Exit(runCreate(os.Args[2:]))
		case "add":
			os.Exit(runAdd(os.Args[2:]))
		case "seal":
			os.Exit(runSeal(os.Args[2:]))
		case "recover":
			os.Exit(runRecover(os.Args[2:]))
		}
	}
	fmt.Fprintf(os.Stderr, `Usage: janus <command>

Commands:
  create    Create an unsealed Transaction
  add       Add a ResourceChange to a Transaction
  seal      Seal a Transaction to begin processing
  recover   Plan or apply manual recovery for a failed transaction
`)
	os.Exit(1)
}

// --- create subcommand ---

func runCreate(args []string) int {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	namespace := fs.StringP("namespace", "n", "default", "namespace")
	sa := fs.String("sa", "", "service account name (required)")
	lockTimeout := fs.String("lock-timeout", "", "per-resource lock timeout (e.g. 5m)")
	timeout := fs.String("timeout", "", "overall transaction timeout (e.g. 30m)")
	generateName := fs.StringP("generate-name", "g", "", "name prefix for server-generated name")
	fs.Parse(args)

	hasName := fs.NArg() > 0
	hasGenerate := *generateName != ""
	if hasName == hasGenerate {
		fmt.Fprintf(os.Stderr, "Usage: janus create <name> --sa <service-account> [-n namespace]\n")
		fmt.Fprintf(os.Stderr, "       janus create -g <prefix> --sa <service-account> [-n namespace]\n")
		fmt.Fprintf(os.Stderr, "\nProvide either a name or --generate-name, not both.\n")
		return 1
	}
	if *sa == "" {
		fmt.Fprintf(os.Stderr, "Error: --sa is required\n")
		return 1
	}

	txn := &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: *namespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			ServiceAccountName: *sa,
		},
	}
	if hasName {
		txn.Name = fs.Arg(0)
	} else {
		txn.GenerateName = *generateName
	}

	if *lockTimeout != "" {
		d, err := parseDuration(*lockTimeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid --lock-timeout: %v\n", err)
			return 1
		}
		txn.Spec.LockTimeout = d
	}
	if *timeout != "" {
		d, err := parseDuration(*timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid --timeout: %v\n", err)
			return 1
		}
		txn.Spec.Timeout = d
	}

	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	if err := cl.Create(ctx, txn); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Transaction: %v\n", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, txn.Name)
	fmt.Fprintf(os.Stderr, "Transaction %s/%s created (unsealed)\n", txn.Namespace, txn.Name)
	return 0
}

// --- add subcommand ---

func runAdd(args []string) int {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	namespace := fs.StringP("namespace", "n", "default", "namespace")
	changeType := fs.String("type", "", "change type: Create, Update, Patch, or Delete (required)")
	target := fs.String("target", "", "target resource as Kind/Name (required)")
	targetNS := fs.String("target-ns", "", "target resource namespace (defaults to transaction namespace)")
	contentFile := fs.StringP("file", "f", "", "path to content YAML file")
	order := fs.Int("order", 0, "execution order (lower executes first)")
	changeName := fs.String("name", "", "ResourceChange name (auto-generated if omitted)")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Usage: janus add <transaction-name> --type <Type> --target <Kind/Name> [-f content.yaml] [--order N] [-n namespace]\n")
		return 1
	}
	if *changeType == "" || *target == "" {
		fmt.Fprintf(os.Stderr, "Error: --type and --target are required\n")
		return 1
	}

	txnName := fs.Arg(0)

	// Parse target Kind/Name.
	parts := strings.SplitN(*target, "/", 2)
	if len(parts) != 2 {
		fmt.Fprintf(os.Stderr, "Error: --target must be Kind/Name (e.g. ConfigMap/my-cm)\n")
		return 1
	}
	kind, name := parts[0], parts[1]

	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	// Load Transaction to get UID and verify it's not sealed.
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: *namespace}, txn); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot find Transaction %q in namespace %q: %v\n", txnName, *namespace, err)
		return 1
	}
	if txn.Spec.Sealed {
		fmt.Fprintf(os.Stderr, "Error: Transaction %q is already sealed\n", txnName)
		return 1
	}

	// Determine target namespace.
	tns := *namespace
	if *targetNS != "" {
		tns = *targetNS
	}

	// Determine apiVersion from kind (best-effort for core resources).
	apiVersion := guessAPIVersion(kind)

	// Read content if provided.
	var content runtime.RawExtension
	if *contentFile != "" {
		data, err := os.ReadFile(*contentFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading content file: %v\n", err)
			return 1
		}
		jsonData, err := yamlToJSON(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error converting content to JSON: %v\n", err)
			return 1
		}
		content = runtime.RawExtension{Raw: jsonData}
	}

	// Auto-generate name if not specified.
	rcName := *changeName
	if rcName == "" {
		rcName = fmt.Sprintf("%s-%s-%s", txnName, strings.ToLower(*changeType), strings.ToLower(name))
	}

	rc := &backupv1alpha1.ResourceChange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rcName,
			Namespace: *namespace,
			Labels: map[string]string{
				"tx.janus.io/transaction": txnName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: backupv1alpha1.GroupVersion.String(),
				Kind:       "Transaction",
				Name:       txn.Name,
				UID:        txn.UID,
			}},
		},
		Spec: backupv1alpha1.ResourceChangeSpec{
			Target: backupv1alpha1.ResourceRef{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       name,
				Namespace:  tns,
			},
			Type:    backupv1alpha1.ChangeType(*changeType),
			Content: content,
			Order:   int32(*order),
		},
	}

	if err := cl.Create(ctx, rc); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating ResourceChange: %v\n", err)
		return 1
	}
	fmt.Printf("ResourceChange %s/%s created (type=%s, target=%s/%s, order=%d)\n",
		rc.Namespace, rc.Name, *changeType, kind, name, *order)
	return 0
}

// --- seal subcommand ---

func runSeal(args []string) int {
	fs := flag.NewFlagSet("seal", flag.ExitOnError)
	namespace := fs.StringP("namespace", "n", "default", "namespace")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Usage: janus seal <transaction-name> [-n namespace]\n")
		return 1
	}
	txnName := fs.Arg(0)

	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	patch := []byte(`{"spec":{"sealed":true}}`)
	txn := &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      txnName,
			Namespace: *namespace,
		},
	}
	if err := cl.Patch(ctx, txn, client.RawPatch(types.MergePatchType, patch)); err != nil {
		fmt.Fprintf(os.Stderr, "Error sealing Transaction: %v\n", err)
		return 1
	}
	fmt.Printf("Transaction %s/%s sealed\n", *namespace, txnName)
	return 0
}

// --- recover subcommand ---

func runRecover(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: janus recover <plan|apply> <transaction-name> [-n namespace]\n")
		return 1
	}

	subcmd := args[0]
	fs := flag.NewFlagSet("recover "+subcmd, flag.ExitOnError)
	namespace := fs.StringP("namespace", "n", "default", "namespace of the transaction")
	force := fs.Bool("force", false, "force apply even on RV conflicts")
	fs.Parse(args[1:])

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Error: transaction name required\n")
		return 1
	}
	txnName := fs.Arg(0)

	switch subcmd {
	case "plan":
		return runRecoverPlan(txnName, *namespace)
	case "apply":
		return runRecoverApply(txnName, *namespace, *force)
	default:
		fmt.Fprintf(os.Stderr, "Unknown recover subcommand: %s\n", subcmd)
		return 1
	}
}

func runRecoverPlan(txnName, namespace string) int {
	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	// Load rollback ConfigMap.
	rbCMName := txnName + "-rollback"
	rbCM := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: rbCMName, Namespace: namespace}, rbCM); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot find rollback ConfigMap %q in namespace %q: %v\n", rbCMName, namespace, err)
		return 1
	}

	// Optionally load Transaction CR for status.
	var txnItems map[string]recover.ItemStatusInfo
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: namespace}, txn); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Warning: cannot read Transaction %q: %v (proceeding from ConfigMap only)\n", txnName, err)
		}
	} else {
		txnItems = make(map[string]recover.ItemStatusInfo, len(txn.Status.Items))
		for _, item := range txn.Status.Items {
			txnItems[item.Name] = recover.ItemStatusInfo{
				Committed:  item.Committed,
				RolledBack: item.RolledBack,
			}
		}
	}

	plan, err := recover.BuildPlan(rbCM, txnItems)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building plan: %v\n", err)
		return 1
	}

	fmt.Print(recover.FormatPlan(plan))
	if plan.HasPending() {
		return 1
	}
	return 0
}

func runRecoverApply(txnName, namespace string, force bool) int {
	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	// Load rollback ConfigMap.
	rbCMName := txnName + "-rollback"
	rbCM := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: rbCMName, Namespace: namespace}, rbCM); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot find rollback ConfigMap %q in namespace %q: %v\n", rbCMName, namespace, err)
		return 1
	}

	// Optionally load Transaction CR for status.
	var txnItems map[string]recover.ItemStatusInfo
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: namespace}, txn); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Warning: cannot read Transaction %q: %v (proceeding from ConfigMap only)\n", txnName, err)
		}
	} else {
		txnItems = make(map[string]recover.ItemStatusInfo, len(txn.Status.Items))
		for _, item := range txn.Status.Items {
			txnItems[item.Name] = recover.ItemStatusInfo{
				Committed:  item.Committed,
				RolledBack: item.RolledBack,
			}
		}
	}

	plan, err := recover.BuildPlan(rbCM, txnItems)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building plan: %v\n", err)
		return 1
	}

	opts := recover.ApplyOptions{Force: force}
	failed := 0
	for _, item := range plan.Items {
		if item.Status != recover.StatusPending {
			continue
		}
		target := fmt.Sprintf("%s/%s/%s", item.Target.Kind, item.Target.Namespace, item.Target.Name)
		fmt.Printf("  - %s %s (%s) ... ", item.Operation, target, item.Name)
		if err := recover.ApplyItem(ctx, cl, item, rbCM, opts); err != nil {
			var conflict *recover.ErrConflict
			if errors.As(err, &conflict) {
				fmt.Printf("CONFLICT (stored RV %s, current RV %s)\n", conflict.StoredRV, conflict.CurrentRV)
			} else {
				fmt.Printf("FAILED: %v\n", err)
			}
			failed++
			continue
		}
		fmt.Println("OK")
	}

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d item(s) failed. Re-run with --force to override conflicts.\n", failed)
		return 1
	}
	fmt.Println("\nAll pending items applied successfully.")
	return 0
}

// --- helpers ---

func buildClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating client: %w", err)
	}
	return cl, nil
}

// parseDuration parses a Go duration string into a metav1.Duration.
func parseDuration(s string) (*metav1.Duration, error) {
	var d metav1.Duration
	if err := d.UnmarshalJSON([]byte(`"` + s + `"`)); err != nil {
		return nil, err
	}
	return &d, nil
}

// guessAPIVersion returns a best-effort apiVersion for common resource kinds.
func guessAPIVersion(kind string) string {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
		return "apps/v1"
	case "Ingress":
		return "networking.k8s.io/v1"
	case "Job", "CronJob":
		return "batch/v1"
	case "HorizontalPodAutoscaler":
		return "autoscaling/v2"
	default:
		return "v1"
	}
}

// yamlToJSON converts YAML bytes to JSON bytes.
func yamlToJSON(data []byte) ([]byte, error) {
	if json.Valid(data) {
		return data, nil
	}
	return sigsyaml.YAMLToJSON(data)
}
