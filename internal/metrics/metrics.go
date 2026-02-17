// Package metrics defines Prometheus metrics for the Janus transaction controller.
// Import this package (blank import) to register metrics on the controller-runtime registry.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// PhaseTransitions counts state machine edge traversals.
	PhaseTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "janus_transaction_phase_transitions_total",
		Help: "Number of transaction phase transitions.",
	}, []string{"from_phase", "to_phase"})

	// Duration observes wall-clock time from StartedAt to CompletedAt by terminal outcome.
	Duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "janus_transaction_duration_seconds",
		Help:    "Transaction duration from start to completion.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s … ~2048s
	}, []string{"outcome"})

	// ActiveTransactions tracks in-flight transactions per non-terminal phase.
	ActiveTransactions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "janus_transactions_active",
		Help: "Number of in-flight transactions per phase.",
	}, []string{"phase"})

	// ItemOperations counts per-item prepare/commit/rollback results.
	ItemOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "janus_item_operations_total",
		Help: "Per-item operation outcomes.",
	}, []string{"operation", "result"})

	// LockOperations counts lock acquire/renew/release results.
	LockOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "janus_lock_operations_total",
		Help: "Lock operation outcomes.",
	}, []string{"operation", "result"})

	// ItemCount observes the distribution of items per transaction.
	ItemCount = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "janus_transaction_item_count",
		Help:    "Number of items per transaction.",
		Buckets: prometheus.LinearBuckets(1, 1, 10), // 1–10 items
	})
)

func init() {
	metrics.Registry.MustRegister(
		PhaseTransitions,
		Duration,
		ActiveTransactions,
		ItemOperations,
		LockOperations,
		ItemCount,
	)
}
