// Package metrics defines and registers Prometheus metrics exported by the
// orchestrator. A single default registry is used so that the standard
// promhttp.Handler exposes everything at /metrics.
//
// Metric naming follows the plan in IMPLEMENTATION_PLAN.md:
//   - orchestrator_jobs_created_total
//   - orchestrator_jobs_completed_total
//   - orchestrator_job_duration_seconds
//   - orchestrator_review_invocations_total
//   - orchestrator_review_tokens_input / _output
//   - orchestrator_queue_depth
//
// Labels are kept deliberately low-cardinality. `pr_number` is intentionally
// high-cardinality — review token metrics are gauges (overwritten) rather
// than counters, so the series churn is bounded by active PRs, not lifetime
// review count.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Terminal job states reported by JobsCompleted.
const (
	JobStatusSucceeded = "succeeded"
	JobStatusFailed    = "failed"
	JobStatusTimeout   = "timeout"
)

// Histogram buckets for job durations. Agent jobs range from a few seconds
// (quick analysis) to the configured task deadline (3600s default). Buckets
// are chosen to give useful resolution across that range.
var jobDurationBuckets = []float64{
	10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200,
}

var (
	// JobsCreated counts agent Jobs created by the lifecycle manager.
	JobsCreated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orchestrator_jobs_created_total",
		Help: "Total number of agent worker Jobs created by the orchestrator.",
	}, []string{"mode", "specialization"})

	// JobsCompleted counts agent Jobs that reached a terminal state.
	// `status` is one of succeeded / failed / timeout.
	JobsCompleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orchestrator_jobs_completed_total",
		Help: "Total number of agent worker Jobs that finished, labeled by terminal status.",
	}, []string{"mode", "specialization", "status"})

	// JobDuration observes the wall-clock time between Job creation and the
	// orchestrator detecting a terminal state.
	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "orchestrator_job_duration_seconds",
		Help:    "Wall-clock time from agent Job creation to terminal state.",
		Buckets: jobDurationBuckets,
	}, []string{"mode", "specialization"})

	// ReviewInvocations counts PR reviews performed by the orchestrator.
	ReviewInvocations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orchestrator_review_invocations_total",
		Help: "Total number of PR reviews invoked by the orchestrator.",
	}, []string{"repo"})

	// ReviewTokensInput tracks input tokens consumed by the most recent
	// review for a given PR. It is a gauge (overwritten per review) so the
	// series set stays bounded by active PRs.
	ReviewTokensInput = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orchestrator_review_tokens_input",
		Help: "Input tokens used by the most recent review of a given PR.",
	}, []string{"repo", "pr_number"})

	// ReviewTokensOutput is the output-token equivalent of ReviewTokensInput.
	ReviewTokensOutput = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orchestrator_review_tokens_output",
		Help: "Output tokens produced by the most recent review of a given PR.",
	}, []string{"repo", "pr_number"})
)

// RegisterQueueDepth installs a GaugeFunc that reports the current queue
// depth by calling `provider` on each scrape. The assignment engine owns
// the queue slice, so reading it through a callback keeps metrics decoupled
// from the engine's internals while still giving a live value.
func RegisterQueueDepth(provider func() int) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "orchestrator_queue_depth",
		Help: "Number of tickets currently waiting in the assignment queue.",
	}, func() float64 {
		return float64(provider())
	})
}
