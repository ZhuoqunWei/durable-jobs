package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
)

type fakeStore struct {
	recover func(context.Context, int) (int, error)
	claim   func(context.Context, time.Duration) (jobs.Claim, error)
	finish  func(context.Context, jobs.Claim, jobs.Outcome) error
}

func (s fakeStore) RecoverExpired(ctx context.Context, limit int) (int, error) {
	if s.recover != nil {
		return s.recover(ctx, limit)
	}
	return 0, nil
}

func (s fakeStore) Claim(ctx context.Context, d time.Duration) (jobs.Claim, error) {
	return s.claim(ctx, d)
}
func (s fakeStore) Finish(ctx context.Context, c jobs.Claim, o jobs.Outcome) error {
	return s.finish(ctx, c, o)
}

type processorFunc func(context.Context, jobs.Job) (jobs.Outcome, error)

func (p processorFunc) Process(ctx context.Context, j jobs.Job) (jobs.Outcome, error) {
	return p(ctx, j)
}

func sampleClaim() jobs.Claim {
	return jobs.Claim{Job: jobs.Job{ID: "test-id", Type: "migrate_campaign", Status: jobs.Running,
		Payload: json.RawMessage(`{"campaign_id":"synthetic_42","category":"Pets","created_at":"08/31/2026"}`)}, Token: "test-token"}
}

func TestWorkerClaimProcessFinish(t *testing.T) {
	claim := sampleClaim()
	finished := false
	store := fakeStore{
		claim: func(ctx context.Context, d time.Duration) (jobs.Claim, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("claim has no deadline")
			}
			if d != 30*time.Second {
				t.Fatalf("lease=%s", d)
			}
			return claim, nil
		},
		finish: func(ctx context.Context, got jobs.Claim, out jobs.Outcome) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("completion has no deadline")
			}
			if got.Token != claim.Token || got.Job.ID != claim.Job.ID || out.Status != jobs.Succeeded || out.Validate() != nil {
				t.Fatalf("finish: %+v %+v", got, out)
			}
			finished = true
			return nil
		},
	}
	w, err := New(store, CampaignProcessor{}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	worked, err := w.RunOnce(context.Background())
	if err != nil || !worked || !finished {
		t.Fatalf("worked=%v finished=%v error=%v", worked, finished, err)
	}
}

func TestWorkerDoesNotProcessUnconfirmedClaims(t *testing.T) {
	for _, claimErr := range []error{jobs.ErrNoQueuedJob, errors.New("claim commit uncertain")} {
		store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) { return jobs.Claim{}, claimErr }}
		w, _ := New(store, processorFunc(func(context.Context, jobs.Job) (jobs.Outcome, error) {
			t.Fatal("processed without claim")
			return jobs.Outcome{}, nil
		}), DefaultConfig())
		worked, err := w.RunOnce(context.Background())
		if worked || (err == nil) != errors.Is(claimErr, jobs.ErrNoQueuedJob) {
			t.Fatalf("worked=%v err=%v", worked, err)
		}
	}
}

func TestWorkerOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		processor processorFunc
		want      jobs.Outcome
		invalid   bool
	}{
		{"domain failure", func(context.Context, jobs.Job) (jobs.Outcome, error) {
			return jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.InvalidCampaign}, nil
		}, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.InvalidCampaign}, false},
		{"private processing error", func(context.Context, jobs.Job) (jobs.Outcome, error) {
			return jobs.Outcome{}, errors.New("secret must not be persisted")
		}, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ProcessingFailed}, false},
		{"cooperative deadline", func(ctx context.Context, _ jobs.Job) (jobs.Outcome, error) {
			<-ctx.Done()
			return jobs.Outcome{}, ctx.Err()
		}, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ProcessingTimeout}, false},
		{"invalid outcome", func(context.Context, jobs.Job) (jobs.Outcome, error) {
			return jobs.Outcome{Status: jobs.Succeeded}, nil
		}, jobs.Outcome{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finished := false
			store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) { return sampleClaim(), nil },
				finish: func(_ context.Context, _ jobs.Claim, out jobs.Outcome) error {
					finished = true
					if out.Status != tc.want.Status || out.FailureCode != tc.want.FailureCode || len(out.Result) != 0 {
						t.Fatalf("outcome=%+v", out)
					}
					return nil
				}}
			config := DefaultConfig()
			config.ProcessingTimeout = 10 * time.Millisecond
			w, err := New(store, tc.processor, config)
			if err != nil {
				t.Fatal(err)
			}
			worked, err := w.RunOnce(context.Background())
			if !worked || finished == tc.invalid || (err != nil) != tc.invalid {
				t.Fatalf("worked=%v finished=%v error=%v", worked, finished, err)
			}
		})
	}
}

func TestWorkerShutdownDoesNotMarkFailure(t *testing.T) {
	for _, cancelDuringClaim := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		processed := false
		store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) {
			if cancelDuringClaim {
				cancel()
			}
			return sampleClaim(), nil
		},
			finish: func(context.Context, jobs.Claim, jobs.Outcome) error {
				t.Fatal("acknowledged shutdown as completion")
				return nil
			}}
		w, _ := New(store, processorFunc(func(context.Context, jobs.Job) (jobs.Outcome, error) {
			processed = true
			cancel()
			return jobs.Outcome{}, context.Canceled
		}), DefaultConfig())
		worked, err := w.RunOnce(ctx)
		cancel()
		if !worked || !errors.Is(err, context.Canceled) || processed == cancelDuringClaim {
			t.Fatalf("worked=%v processed=%v error=%v", worked, processed, err)
		}
	}
}

func TestWorkerCompletionErrorIsReturned(t *testing.T) {
	store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) { return sampleClaim(), nil },
		finish: func(context.Context, jobs.Claim, jobs.Outcome) error { return jobs.ErrLeaseLost }}
	w, _ := New(store, CampaignProcessor{}, DefaultConfig())
	worked, err := w.RunOnce(context.Background())
	if !worked || !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("worked=%v error=%v", worked, err)
	}
}

func TestWorkerPollCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) {
		calls++
		cancel()
		return jobs.Claim{}, jobs.ErrNoQueuedJob
	}}
	w, _ := New(store, CampaignProcessor{}, DefaultConfig())
	if err := w.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	worked, err := w.RunOnce(ctx)
	if worked || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("pre-canceled worker touched storage")
	}
}

func TestWorkerConfigValidation(t *testing.T) {
	for _, config := range []Config{{}, {LeaseDuration: time.Second, ProcessingTimeout: time.Second, StorageTimeout: time.Second, PollInterval: time.Second},
		{LeaseDuration: time.Second, ProcessingTimeout: 500 * time.Millisecond, StorageTimeout: 300 * time.Millisecond, PollInterval: time.Second}} {
		if _, err := New(fakeStore{}, CampaignProcessor{}, config); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
	if _, err := New(nil, CampaignProcessor{}, DefaultConfig()); err == nil {
		t.Fatal("accepted nil store")
	}
	if _, err := New(fakeStore{}, nil, DefaultConfig()); err == nil {
		t.Fatal("accepted nil processor")
	}
}

func TestCampaignProcessorFailures(t *testing.T) {
	for _, tc := range []struct {
		job  jobs.Job
		code string
	}{
		{jobs.Job{Type: "unknown"}, jobs.UnsupportedJobType},
		{jobs.Job{Type: "migrate_campaign", Payload: json.RawMessage(`{}`)}, jobs.InvalidCampaign},
	} {
		out, err := (CampaignProcessor{}).Process(context.Background(), tc.job)
		if err != nil || out.Status != jobs.Failed || out.FailureCode != tc.code {
			t.Fatalf("%+v %v", out, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (CampaignProcessor{}).Process(ctx, sampleClaim().Job); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
