// Package telemetry holds Prometheus metrics, OpenTelemetry tracing and structured logging.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry is the process-wide registry exposed on /metrics.
var Registry = prometheus.NewRegistry()

var factory = func() prometheus.Registerer {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return Registry
}()

// Metrics named in spec §14 and the §4.7 degradation signals.
var (
	ScoreLatency = register(prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "fraud_score_latency_seconds", Help: "Score end-to-end latency.",
		Buckets: []float64{.001, .002, .005, .0075, .01, .015, .02, .025, .03, .035, .04, .045, .05, .075, .1, .25},
	}))
	DecisionsTotal = register(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fraud_decisions_total", Help: "Decisions by action.",
	}, []string{"action"}))
	DegradedTotal = register(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fraud_degraded_total", Help: "Degraded decisions by reason (model, features).",
	}, []string{"reason"}))
	UpdaterLag = register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fraud_updater_lag_seconds", Help: "Age of the last event applied, per partition.",
	}, []string{"partition"}))
	UpdaterEvents = register(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fraud_updater_events_total", Help: "Events consumed by the feature-updater.",
	}, []string{"topic", "result"}))
	DLQTotal = register(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fraud_dlq_total", Help: "Messages routed to events.dlq.",
	}, []string{"source_topic"}))
	SpoolDepth = register(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "fraud_spool_depth", Help: "Decisions waiting in the disk spool.",
	}))
	OutboxUnsent = register(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "fraud_outbox_unsent", Help: "Unsent decision_outbox rows.",
	}))
	OutboxOldestUnsentAge = register(prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "fraud_outbox_oldest_unsent_age_seconds", Help: "Age of the oldest unsent outbox row.",
	}))
	ShadowDropped = register(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fraud_shadow_dropped_total", Help: "Shadow scoring jobs dropped because the queue was full.",
	}))
	ShadowCompared = register(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fraud_shadow_comparisons_total", Help: "Shadow comparisons by agreement.",
	}, []string{"agree"}))
	PublishDropped = register(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fraud_publish_dropped_total", Help: "events.transaction messages dropped (producer buffer full).",
	}))
	AccountEventsDropped = register(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fraud_account_events_dropped_total", Help: "account_events rows dropped (writer queue full).",
	}))
	AccountEventsFailed = register(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fraud_account_events_insert_failed_total", Help: "account_events rows lost because a batch insert failed.",
	}))
	RulesVersion = register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fraud_rules_version_info", Help: "Active rule-set version (value 1).",
	}, []string{"version"}))
)

func register[T prometheus.Collector](c T) T {
	factory.MustRegister(c)
	return c
}
