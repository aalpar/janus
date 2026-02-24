// Package recover implements offline recovery for failed or interrupted
// Janus transactions by reading rollback state from ConfigMaps.
package recover

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aalpar/janus/internal/rollback"
	corev1 "k8s.io/api/core/v1"
)

// ItemStatus describes the rollback status of a single plan item.
type ItemStatus string

const (
	StatusPending  ItemStatus = "pending"
	StatusDone     ItemStatus = "done"
	StatusConflict ItemStatus = "conflict"
)

// PlanItem represents one rollback operation in the recovery plan.
type PlanItem struct {
	Name      string // name of the ResourceChange CR
	Target    rollback.MetaTarget
	Operation string // "DELETE" (reverse of Create) or "RESTORE" (reverse of Update/Patch/Delete)
	Status    ItemStatus
	StoredRV  string
	CurrentRV string // populated during live plan (when checking against cluster)
	Reason    string // human-readable explanation for conflict status
}

// Plan is the full recovery plan for a transaction.
type Plan struct {
	TransactionName      string
	TransactionNamespace string
	Items                []PlanItem
}

// ItemStatusInfo is a subset of the Transaction's ItemStatus relevant to recovery.
type ItemStatusInfo struct {
	Committed  bool
	RolledBack bool
}

// BuildPlan constructs a recovery plan from the rollback ConfigMap.
// txnItems is optional -- if nil, all items are treated as pending rollback.
// The map is keyed by ResourceChange CR name.
func BuildPlan(rbCM *corev1.ConfigMap, txnItems map[string]ItemStatusInfo) (*Plan, error) {
	rawMeta, ok := rbCM.Data[rollback.MetaKey]
	if !ok {
		return nil, fmt.Errorf("rollback ConfigMap missing %s key", rollback.MetaKey)
	}
	var meta rollback.Meta
	if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
		return nil, fmt.Errorf("parsing _meta: %w", err)
	}

	plan := &Plan{
		TransactionName:      meta.TransactionName,
		TransactionNamespace: meta.TransactionNamespace,
	}

	for _, ch := range meta.Changes {
		var env rollback.Envelope
		if raw, ok := rbCM.Data[ch.RollbackKey]; ok {
			if err := json.Unmarshal([]byte(raw), &env); err != nil {
				return nil, fmt.Errorf("parsing envelope for %s: %w", ch.RollbackKey, err)
			}
		}

		op := "RESTORE"
		if ch.ChangeType == "Create" {
			op = "DELETE"
		}

		status := StatusPending
		if info, ok := txnItems[ch.Name]; ok {
			if info.RolledBack {
				status = StatusDone
			}
			if !info.Committed {
				status = StatusDone // never committed, nothing to roll back
			}
		}

		plan.Items = append(plan.Items, PlanItem{
			Name:      ch.Name,
			Target:    ch.Target,
			Operation: op,
			Status:    status,
			StoredRV:  env.ResourceVersion,
		})
	}

	return plan, nil
}

// FormatPlan produces a human-readable plan string.
func FormatPlan(p *Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Transaction: %s (namespace: %s)\n\n", p.TransactionName, p.TransactionNamespace)
	fmt.Fprintf(&b, "Rollback plan:\n")
	for _, item := range p.Items {
		target := fmt.Sprintf("%s/%s/%s", item.Target.Kind, item.Target.Namespace, item.Target.Name)
		switch item.Status {
		case StatusDone:
			fmt.Fprintf(&b, "  - [done]     %s %s (%s)\n", item.Operation, target, item.Name)
		case StatusConflict:
			fmt.Fprintf(&b, "  - [conflict] %s %s (%s)\n", item.Operation, target, item.Name)
			if item.Reason != "" {
				fmt.Fprintf(&b, "               %s\n", item.Reason)
			}
		default:
			fmt.Fprintf(&b, "  - [pending]  %s %s (%s)\n", item.Operation, target, item.Name)
		}
	}
	return b.String()
}

// HasPending returns true if any items still need rollback.
func (p *Plan) HasPending() bool {
	for _, item := range p.Items {
		if item.Status == StatusPending || item.Status == StatusConflict {
			return true
		}
	}
	return false
}
