package telemetry

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrometheusDurableMetrics(t *testing.T) {
	s := Snapshot{Queued: 2, Retrying: 1, Failed: 1, StartCount: 2, StartSum: 4, CompletionCount: 1,
		StartBuckets: []int64{0, 0, 1, 1, 2, 2, 2, 2, 2}, CompletionBuckets: []int64{0, 0, 0, 0, 1, 1, 1, 1, 1},
		StartEligible: 3, StartGood: 2, CompletionEligible: 3, CompletionGood: 1, HeartbeatAge: -1}
	var b bytes.Buffer
	Write(&b, s)
	for _, want := range []string{"durable_jobs_queue_depth 3", "durable_jobs_start_latency_seconds_bucket{le=\"+Inf\"} 2",
		"durable_jobs_start_latency_seconds_bucket{le=\"5\"} 2", "durable_jobs_start_latency_seconds_sum 4",
		"durable_jobs_slo_start_eligible 3", "durable_jobs_slo_completion_good 1", "durable_jobs_worker_heartbeat_age_seconds -1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(b.String(), "job_id") {
		t.Fatal("unbounded label")
	}
}
