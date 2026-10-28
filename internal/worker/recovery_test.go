package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
)

func TestWorkerRecoversBeforeClaiming(t *testing.T) {
	recovered := false
	store := fakeStore{recover: func(ctx context.Context, limit int) (int, error) {
		if _, ok := ctx.Deadline(); !ok || limit != 10 {
			t.Fatal("unbounded recovery")
		}
		recovered = true
		return 1, nil
	}, claim: func(context.Context, time.Duration) (jobs.Claim, error) {
		if !recovered {
			t.Fatal("claimed before recovery")
		}
		return jobs.Claim{}, jobs.ErrNoQueuedJob
	}}
	w, err := New(store, CampaignProcessor{}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := w.RunOnce(context.Background())
	if err != nil || worked || !recovered {
		t.Fatalf("worked=%v recovered=%v err=%v", worked, recovered, err)
	}
}

func TestRecoveryFailureDoesNotClaim(t *testing.T) {
	sentinel := errors.New("recovery commit uncertain")
	store := fakeStore{recover: func(context.Context, int) (int, error) { return 0, sentinel }, claim: func(context.Context, time.Duration) (jobs.Claim, error) {
		t.Fatal("claimed after failed recovery")
		return jobs.Claim{}, nil
	}}
	w, _ := New(store, CampaignProcessor{}, DefaultConfig())
	worked, err := w.RunOnce(context.Background())
	if worked || !errors.Is(err, sentinel) {
		t.Fatalf("%v %v", worked, err)
	}
}

func TestExplicitTransientProcessingFailure(t *testing.T) {
	finished := false
	store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) { return sampleClaim(), nil }, finish: func(_ context.Context, _ jobs.Claim, out jobs.Outcome) error {
		finished = true
		if out.Status != jobs.Failed || out.FailureCode != jobs.TransientFailure {
			t.Fatalf("outcome=%+v", out)
		}
		return nil
	}}
	p := processorFunc(func(context.Context, jobs.Job) (jobs.Outcome, error) {
		return jobs.Outcome{}, errors.Join(jobs.ErrTransient, errors.New("private upstream detail"))
	})
	w, _ := New(store, p, DefaultConfig())
	if worked, err := w.RunOnce(context.Background()); !worked || err != nil || !finished {
		t.Fatalf("%v %v", worked, err)
	}
}
