# generateName Support Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make `generateName` a first-class Transaction creation mode alongside explicit names.

**Architecture:** CLI-only changes — the controller already uses UID-based ownership and computes all name-derived values post-creation. Changes: `runCreate` gets `--generate-name` flag + stdout/stderr split; `runAdd` uses fetched `txn.Name`; `runRecoverPlan`/`runRecoverApply` use `txn.Status.RollbackRef`; docs updated.

**Tech Stack:** Go, pflag, controller-runtime client

---

### Task 1: `runCreate` — add `--generate-name` flag and stdout/stderr split

**Files:**
- Modify: `cmd/janus/main.go:72-129` (`runCreate`)

**Step 1: Modify `runCreate`**

Replace the current `runCreate` function body. Changes:
1. Add `--generate-name` / `-g` string flag
2. Replace the `fs.NArg() == 0` check with mutual exclusion logic: exactly one of positional name or `--generate-name` must be set
3. Set `ObjectMeta.GenerateName` or `ObjectMeta.Name` accordingly
4. Split output: resolved name to stdout, human message to stderr

```go
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
```

**Step 2: Build**

Run: `go build ./cmd/janus/`
Expected: clean build, no errors

**Step 3: Commit**

```
feat: add --generate-name flag to janus create

Supports server-generated Transaction names via -g/--generate-name prefix.
Output split: resolved name to stdout, human message to stderr, making
TXN=$(janus create ...) work uniformly in both modes.
```

---

### Task 2: `runAdd` — use fetched `txn.Name` for RC auto-naming

**Files:**
- Modify: `cmd/janus/main.go:206-210` (inside `runAdd`)

**Step 1: Change RC auto-name derivation**

At line 209, change `txnName` to `txn.Name`:

```go
// Before:
rcName = fmt.Sprintf("%s-%s-%s", txnName, strings.ToLower(*changeType), strings.ToLower(name))

// After:
rcName = fmt.Sprintf("%s-%s-%s", txn.Name, strings.ToLower(*changeType), strings.ToLower(name))
```

Also change the label at line 217 to use `txn.Name`:

```go
// Before:
"tx.janus.io/transaction": txnName,

// After:
"tx.janus.io/transaction": txn.Name,
```

These are functionally identical today (the GET would fail if they differed),
but using the fetched object is canonical.

**Step 2: Build**

Run: `go build ./cmd/janus/`
Expected: clean build

**Step 3: Commit**

```
refactor: use fetched txn.Name in janus add for RC auto-naming

Use the authoritative name from the fetched Transaction object rather
than the CLI argument for ResourceChange auto-naming and labels.
```

---

### Task 3: `runRecoverPlan` — use `txn.Status.RollbackRef`

**Files:**
- Modify: `cmd/janus/main.go:314-357` (`runRecoverPlan`)

**Step 1: Restructure to fetch Transaction first**

The current flow fetches the rollback ConfigMap by hardcoded name, then
optionally fetches the Transaction. Reverse the order: fetch the Transaction
first to get `RollbackRef`, fall back to the hardcoded derivation only if the
Transaction is not found.

```go
func runRecoverPlan(txnName, namespace string) int {
	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	// Load Transaction CR for status and RollbackRef.
	var txnItems map[string]recover.ItemStatusInfo
	rbCMName := txnName + "-rollback" // fallback if Transaction CR is gone
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: namespace}, txn); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Warning: cannot read Transaction %q: %v (proceeding from ConfigMap only)\n", txnName, err)
		}
	} else {
		if txn.Status.RollbackRef != "" {
			rbCMName = txn.Status.RollbackRef
		}
		txnItems = make(map[string]recover.ItemStatusInfo, len(txn.Status.Items))
		for _, item := range txn.Status.Items {
			txnItems[item.Name] = recover.ItemStatusInfo{
				Committed:  item.Committed,
				RolledBack: item.RolledBack,
			}
		}
	}

	// Load rollback ConfigMap.
	rbCM := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: rbCMName, Namespace: namespace}, rbCM); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot find rollback ConfigMap %q in namespace %q: %v\n", rbCMName, namespace, err)
		return 1
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
```

**Step 2: Build**

Run: `go build ./cmd/janus/`
Expected: clean build

**Step 3: Commit**

```
fix: use txn.Status.RollbackRef in recover plan

Read the rollback ConfigMap name from the Transaction's status instead
of hardcoding the derivation. Falls back to name + "-rollback" when the
Transaction CR is not found (e.g. already deleted).
```

---

### Task 4: `runRecoverApply` — same RollbackRef fix

**Files:**
- Modify: `cmd/janus/main.go:360-426` (`runRecoverApply`)

**Step 1: Apply the same restructure as Task 3**

```go
func runRecoverApply(txnName, namespace string, force bool) int {
	ctx := context.Background()
	cl, err := buildClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building client: %v\n", err)
		return 1
	}

	// Load Transaction CR for status and RollbackRef.
	var txnItems map[string]recover.ItemStatusInfo
	rbCMName := txnName + "-rollback" // fallback if Transaction CR is gone
	txn := &backupv1alpha1.Transaction{}
	if err := cl.Get(ctx, client.ObjectKey{Name: txnName, Namespace: namespace}, txn); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "Warning: cannot read Transaction %q: %v (proceeding from ConfigMap only)\n", txnName, err)
		}
	} else {
		if txn.Status.RollbackRef != "" {
			rbCMName = txn.Status.RollbackRef
		}
		txnItems = make(map[string]recover.ItemStatusInfo, len(txn.Status.Items))
		for _, item := range txn.Status.Items {
			txnItems[item.Name] = recover.ItemStatusInfo{
				Committed:  item.Committed,
				RolledBack: item.RolledBack,
			}
		}
	}

	// Load rollback ConfigMap.
	rbCM := &corev1.ConfigMap{}
	if err := cl.Get(ctx, client.ObjectKey{Name: rbCMName, Namespace: namespace}, rbCM); err != nil {
		fmt.Fprintf(os.Stderr, "Error: cannot find rollback ConfigMap %q in namespace %q: %v\n", rbCMName, namespace, err)
		return 1
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
```

**Step 2: Build**

Run: `go build ./cmd/janus/`
Expected: clean build

**Step 3: Commit**

```
fix: use txn.Status.RollbackRef in recover apply

Same fix as recover plan — read rollback ConfigMap name from Transaction
status with fallback derivation.
```

---

### Task 5: Update USER_GUIDE.md

**Files:**
- Modify: `docs/USER_GUIDE.md`

**Step 1: Add generateName to "Create the Transaction" section**

After the existing `janus create deploy-v2 --sa janus-txn` example (line 70),
add a generateName variant:

```markdown
Or with a server-generated name:

```sh
TXN=$(janus create -g deploy- --sa janus-txn [-n default])
```

The resolved name is printed to stdout, making it easy to capture for
subsequent commands:

```sh
TXN=$(janus create -g deploy- --sa janus-txn)
janus add "$TXN" --type Patch --target ConfigMap/app-config -f patch.yaml
janus seal "$TXN"
```
```

**Step 2: Add a generateName variant to the multi-operation example**

After the existing multi-operation example block ending at line 532, add:

```markdown
With a generated name:

```sh
TXN=$(janus create -g deploy-v2- --sa janus-txn)

janus add "$TXN" --type Patch --target ConfigMap/app-config \
  -f config-patch.yaml

janus add "$TXN" --type Patch --target Deployment/web-server \
  -f deploy-patch.yaml --order 1

janus add "$TXN" --type Delete --target Secret/old-api-key \
  --order 2

janus seal "$TXN"
```
```

**Step 3: Build (verify no doc syntax issues)**

Run: `go build ./cmd/janus/`
Expected: clean build (sanity check that no Go files were accidentally touched)

**Step 4: Commit**

```
docs: add generateName examples to user guide
```
