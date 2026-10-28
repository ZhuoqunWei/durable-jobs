package database

import (
	"context"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
)

func TestMetricsAndHeartbeatIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := NewJobStore(pool)
	empty, err := s.Metrics(ctx)
	if err != nil || empty.HeartbeatAge != -1 || empty.StartEligible != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	if err = s.Heartbeat(ctx, "synthetic-worker"); err != nil {
		t.Fatal(err)
	}
	if err = s.Heartbeat(ctx, "synthetic-worker"); err != nil {
		t.Fatal(err)
	}
	if err = s.Heartbeat(ctx, ""); err == nil {
		t.Fatal("accepted empty identity")
	}
	j, err := s.Create(ctx, submission(validCampaign), "metrics-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "UPDATE jobs SET created_at=clock_timestamp()-interval '70 seconds' WHERE id=$1", j.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.Metrics(ctx)
	if err != nil || before.Queued != 1 || before.StartEligible != 1 || before.StartGood != 0 || before.CompletionEligible != 1 || before.CompletionGood != 0 || before.ActiveWorkers != 1 {
		t.Fatalf("pending jobs must count: %+v %v", before, err)
	}
	claim, err := s.Claim(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, claim, jobs.Outcome{Status: jobs.Succeeded, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Metrics(ctx)
	if err != nil || after.Succeeded != 1 || after.StartCount != 1 || after.CompletionCount != 1 || after.StartGood != 0 || after.CompletionGood != 0 || after.StartBuckets[8] != 1 || after.StartBuckets[7] != 0 {
		t.Fatalf("late job: %+v %v", after, err)
	}
}
