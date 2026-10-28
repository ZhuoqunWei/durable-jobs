package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"durable-jobs/internal/telemetry"
)

func (s *JobStore) Heartbeat(ctx context.Context, id string) error {
	if len(id) == 0 || len(id) > 128 {
		return errors.New("invalid worker heartbeat ID")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO worker_heartbeats(worker_id) VALUES($1)
		ON CONFLICT(worker_id) DO UPDATE SET seen_at=clock_timestamp()`, id)
	return err
}

func (s *JobStore) Metrics(ctx context.Context) (telemetry.Snapshot, error) {
	var out telemetry.Snapshot
	// A single statement snapshot and DB clock avoid replica/process-clock skew.
	var start, complete []string
	for _, bound := range telemetry.Buckets {
		start = append(start, fmt.Sprintf("count(*) FILTER (WHERE started_at-created_at <= interval '%g seconds')", bound))
		complete = append(complete, fmt.Sprintf("count(*) FILTER (WHERE completed_at-created_at <= interval '%g seconds')", bound))
	}
	query := `SELECT count(*) FILTER (WHERE status='queued'), count(*) FILTER (WHERE status='running'),
		count(*) FILTER (WHERE status='retry_waiting'), count(*) FILTER (WHERE status='succeeded'),
		count(*) FILTER (WHERE status='failed'), count(started_at), count(completed_at),
		coalesce(sum(extract(epoch FROM started_at-created_at)),0)::float8,
		coalesce(sum(extract(epoch FROM completed_at-created_at)),0)::float8,
		ARRAY[` + strings.Join(start, ",") + `], ARRAY[` + strings.Join(complete, ",") + `],
		count(*) FILTER (WHERE created_at <= statement_timestamp()-interval '5 seconds'),
		count(*) FILTER (WHERE created_at <= statement_timestamp()-interval '5 seconds' AND started_at-created_at <= interval '5 seconds'),
		count(*) FILTER (WHERE created_at <= statement_timestamp()-interval '60 seconds'),
		count(*) FILTER (WHERE created_at <= statement_timestamp()-interval '60 seconds' AND completed_at-created_at <= interval '60 seconds'),
		(SELECT coalesce(extract(epoch FROM statement_timestamp()-max(seen_at)),-1)::float8 FROM worker_heartbeats),
		(SELECT count(*) FROM worker_heartbeats WHERE seen_at >= statement_timestamp()-interval '15 seconds')
		FROM jobs`
	err := s.pool.QueryRow(ctx, query).Scan(&out.Queued, &out.Running, &out.Retrying, &out.Succeeded, &out.Failed,
		&out.StartCount, &out.CompletionCount, &out.StartSum, &out.CompletionSum, &out.StartBuckets, &out.CompletionBuckets,
		&out.StartEligible, &out.StartGood, &out.CompletionEligible, &out.CompletionGood, &out.HeartbeatAge, &out.ActiveWorkers)
	return out, err
}
