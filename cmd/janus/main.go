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
	"errors"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/recover"
	"github.com/aalpar/janus/internal/scheme"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "recover":
			os.Exit(runRecover(os.Args[2:]))
		}
	}
	fmt.Fprintf(os.Stderr, "Usage: janus <command>\n\nCommands:\n  recover   Plan or apply manual recovery for a failed transaction\n")
	os.Exit(1)
}

func runRecover(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: janus recover <plan|apply> <transaction-name> [-n namespace]\n")
		return 1
	}

	subcmd := args[0]
	fs := flag.NewFlagSet("recover "+subcmd, flag.ExitOnError)
	namespace := fs.String("n", "default", "namespace of the transaction")
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
	var txnItems []recover.ItemStatusInfo
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: namespace}, txn); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Warning: cannot read Transaction %q: %v (proceeding from ConfigMap only)\n", txnName, err)
		}
	} else {
		txnItems = make([]recover.ItemStatusInfo, len(txn.Status.Items))
		for i, item := range txn.Status.Items {
			txnItems[i] = recover.ItemStatusInfo{
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
	var txnItems []recover.ItemStatusInfo
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: namespace}, txn); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Warning: cannot read Transaction %q: %v (proceeding from ConfigMap only)\n", txnName, err)
		}
	} else {
		txnItems = make([]recover.ItemStatusInfo, len(txn.Status.Items))
		for i, item := range txn.Status.Items {
			txnItems[i] = recover.ItemStatusInfo{
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
		fmt.Printf("  %d. %s %s ... ", item.Index, item.Operation, target)
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
