// Package telemetry exposes database-backed, bounded-cardinality lab metrics.
// These lifetime counters reset if the database is replaced, not on API restart.
package telemetry

import (
	"fmt"
	"io"
)

var Buckets = []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120}

type Snapshot struct {
	Queued, Running, Retrying, Succeeded, Failed                 int64
	StartCount, CompletionCount                                  int64
	StartSum, CompletionSum                                      float64
	StartBuckets, CompletionBuckets                              []int64
	StartEligible, StartGood, CompletionEligible, CompletionGood int64
	HeartbeatAge                                                 float64
	ActiveWorkers                                                int64
}

func Write(w io.Writer, s Snapshot) error {
	if len(s.StartBuckets) != len(Buckets) || len(s.CompletionBuckets) != len(Buckets) {
		return fmt.Errorf("incomplete metric histogram")
	}
	gauge := func(name, help string, value any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, value)
	}
	gauge("durable_jobs_queue_depth", "Queued plus retry-waiting jobs.", s.Queued+s.Retrying)
	gauge("durable_jobs_running", "Jobs with an active or expired unrecovered lease.", s.Running)
	fmt.Fprintln(w, "# HELP durable_jobs_terminal_total Terminal jobs since this database was created.\n# TYPE durable_jobs_terminal_total counter")
	fmt.Fprintf(w, "durable_jobs_terminal_total{outcome=\"succeeded\"} %d\ndurable_jobs_terminal_total{outcome=\"failed\"} %d\n", s.Succeeded, s.Failed)
	histogram := func(name string, count int64, sum float64, values []int64) {
		fmt.Fprintf(w, "# HELP %s Database timestamp duration, seconds.\n# TYPE %s histogram\n", name, name)
		for i, bound := range Buckets {
			fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, bound, values[i])
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, count, name, sum, name, count)
	}
	histogram("durable_jobs_start_latency_seconds", s.StartCount, s.StartSum, s.StartBuckets)
	histogram("durable_jobs_completion_latency_seconds", s.CompletionCount, s.CompletionSum, s.CompletionBuckets)
	gauge("durable_jobs_slo_start_eligible", "Accepted jobs at least 5 seconds old; unfinished jobs remain in denominator.", s.StartEligible)
	gauge("durable_jobs_slo_start_good", "Eligible accepted jobs first started within 5 seconds.", s.StartGood)
	gauge("durable_jobs_slo_completion_eligible", "Accepted jobs at least 60 seconds old, including failures and unfinished jobs.", s.CompletionEligible)
	gauge("durable_jobs_slo_completion_good", "Eligible accepted jobs reached a terminal state within 60 seconds.", s.CompletionGood)
	gauge("durable_jobs_worker_heartbeat_age_seconds", "Newest committed worker heartbeat age; -1 if none exists.", s.HeartbeatAge)
	gauge("durable_jobs_active_workers", "Worker heartbeats committed within 15 seconds; not proof of job progress.", s.ActiveWorkers)
	return nil
}
