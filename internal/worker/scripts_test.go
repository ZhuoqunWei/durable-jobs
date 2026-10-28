package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/sandbox"
	"durable-jobs/internal/scripts"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeRunner struct {
	result  sandbox.Result
	image   string
	calls   int
	request sandbox.Request
}

func (r *fakeRunner) ImageID() string { return r.image }
func (r *fakeRunner) Run(_ context.Context, request sandbox.Request) sandbox.Result {
	r.calls++
	r.request = request
	return r.result
}

func scriptJobForTest() jobs.Job {
	payload, _ := json.Marshal(scripts.Payload{Script: scripts.CampaignName, Version: scripts.CampaignVersion, SourceSHA256: scripts.CampaignSHA256, ImageID: testImage, Input: json.RawMessage(`{"synthetic":true}`)})
	return jobs.Job{Type: "run_script", Payload: payload}
}

func TestVersionedProcessorOutcomes(t *testing.T) {
	for _, tc := range []struct {
		code             string
		want             string
		transient, fatal bool
	}{
		{"script_failed", jobs.ScriptFailed, false, false}, {"invalid_output", jobs.ScriptFailed, false, false},
		{"time_limit", jobs.ScriptLimitExceeded, false, false}, {"memory_limit", jobs.ScriptLimitExceeded, false, false},
		{"output_limit", jobs.ScriptLimitExceeded, false, false},
		{"runner_unavailable", "", true, false}, {"container_create_failed", "", true, false},
		{"container_state_unknown", "", true, false}, {"container_attach_failed", "", true, false},
		{"cleanup_failed", jobs.SandboxCleanupFailed, false, true}, {"private unexpected failure", jobs.ScriptFailed, false, false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			r := &fakeRunner{image: testImage, result: sandbox.Result{Status: "failed", FailureCode: tc.code, CleanupConfirmed: !tc.fatal}}
			p := VersionedProcessor{NewRunner: func(context.Context) (ScriptRunner, error) { return r, nil }}
			out, err := p.Process(context.Background(), scriptJobForTest())
			if out.FailureCode != tc.want || errors.Is(err, jobs.ErrTransient) != tc.transient || errors.Is(err, ErrStopWorker) != tc.fatal || r.calls != 1 {
				t.Fatalf("out=%+v err=%v calls=%d", out, err, r.calls)
			}
			if !tc.transient && (out.Validate() != nil || out.Status != jobs.Failed) {
				t.Fatal("invalid outcome", out)
			}
			if !tc.transient {
				if state, _ := jobs.AfterFailure(out.FailureCode, 1, 3); state != jobs.Failed {
					t.Fatal("permanent script failure retried")
				}
			}
		})
	}
}

func TestVersionedProcessorSuccessAndPins(t *testing.T) {
	good := sandbox.Result{Status: "succeeded", Output: json.RawMessage(`{"synthetic":true}`), CleanupConfirmed: true, ImageID: testImage, ScriptSHA256: scripts.CampaignSHA256}
	for _, tc := range []struct {
		name   string
		change func(*fakeRunner)
		want   string
		calls  int
	}{
		{"success", func(*fakeRunner) {}, "", 1},
		{"retagged image", func(r *fakeRunner) { r.image = "sha256:" + strings.Repeat("b", 64) }, jobs.ScriptVersionUnavailable, 0},
		{"wrong executed source", func(r *fakeRunner) { r.result.ScriptSHA256 = "wrong" }, jobs.ScriptVersionUnavailable, 1},
		{"wrong executed image", func(r *fakeRunner) { r.result.ImageID = "wrong" }, jobs.ScriptVersionUnavailable, 1},
		{"invalid output", func(r *fakeRunner) { r.result.Output = json.RawMessage(`[]`) }, jobs.ScriptFailed, 1},
		{"metadata exceeds bound", func(r *fakeRunner) {
			r.result.Output = json.RawMessage(`{"x":"` + strings.Repeat("a", (64<<10)-8) + `"}`)
		}, jobs.ScriptLimitExceeded, 1},
		{"success but cleanup unknown", func(r *fakeRunner) { r.result.CleanupConfirmed = false }, jobs.SandboxCleanupFailed, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{image: testImage, result: good}
			tc.change(r)
			p := VersionedProcessor{NewRunner: func(context.Context) (ScriptRunner, error) { return r, nil }}
			out, err := p.Process(context.Background(), scriptJobForTest())
			if out.FailureCode != tc.want || r.calls != tc.calls || out.Validate() != nil {
				t.Fatalf("out=%+v err=%v calls=%d", out, err, r.calls)
			}
			if tc.want == "" {
				var stored map[string]json.RawMessage
				if err != nil || out.Status != jobs.Succeeded || json.Unmarshal(out.Result, &stored) != nil || string(stored["output"]) != `{"synthetic":true}` || string(stored["cleanup_confirmed"]) != "true" {
					t.Fatal(out, err)
				}
				source, _ := scripts.Resolve(scripts.Payload{Script: scripts.CampaignName, Version: scripts.CampaignVersion, SourceSHA256: scripts.CampaignSHA256})
				if r.request.Script != source || string(r.request.Input) != `{"synthetic":true}` {
					t.Fatal("wrong script/input sent")
				}
			}
		})
	}
	noDocker := VersionedProcessor{NewRunner: func(context.Context) (ScriptRunner, error) { t.Fatal("should not initialize Docker"); return nil, nil }}
	if out, err := noDocker.Process(context.Background(), sampleClaim().Job); err != nil || out.Status != jobs.Succeeded {
		t.Fatal(out, err)
	}
	job := scriptJobForTest()
	job.Payload = json.RawMessage(strings.Replace(string(job.Payload), `"version":"1"`, `"version":"2"`, 1))
	if out, _ := noDocker.Process(context.Background(), job); out.FailureCode != jobs.ScriptVersionUnavailable {
		t.Fatal(out)
	}
	unavailable := VersionedProcessor{NewRunner: func(context.Context) (ScriptRunner, error) { return nil, errors.New("private Docker diagnostic") }}
	if _, err := unavailable.Process(context.Background(), scriptJobForTest()); !errors.Is(err, jobs.ErrTransient) {
		t.Fatal(err)
	}
}

func TestUnsafeCleanupStopsWorkerEvenAfterCancellation(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		for _, finishFails := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			claims, finishes := 0, 0
			store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) { claims++; return sampleClaim(), nil },
				finish: func(ctx context.Context, _ jobs.Claim, out jobs.Outcome) error {
					finishes++
					if ctx.Err() != nil || out.FailureCode != jobs.SandboxCleanupFailed || out.Validate() != nil {
						t.Fatal("lost cleanup failure", out, ctx.Err())
					}
					if finishFails {
						return jobs.ErrLeaseLost
					}
					return nil
				}}
			processor := processorFunc(func(processCtx context.Context, _ jobs.Job) (jobs.Outcome, error) {
				if shutdown {
					cancel()
				} else {
					<-processCtx.Done()
				}
				return jobs.Outcome{}, ErrStopWorker
			})
			config := ScriptConfig()
			config.ProcessingTimeout = time.Millisecond
			w, err := New(store, processor, config)
			if err != nil {
				t.Fatal(err)
			}
			err = w.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
			cancel()
			if !errors.Is(err, ErrStopWorker) || errors.Is(err, jobs.ErrLeaseLost) != finishFails || claims != 1 || finishes != 1 {
				t.Fatalf("error=%v claims=%d finishes=%d", err, claims, finishes)
			}
		}
	}
}

func TestScriptConfigReservesCleanup(t *testing.T) {
	c := ScriptConfig()
	if c.CleanupGrace < sandbox.CleanupGrace {
		t.Fatal("no cleanup reservation")
	}
	if _, err := New(fakeStore{}, CampaignProcessor{}, c); err != nil {
		t.Fatal(err)
	}
	c.LeaseDuration = c.ProcessingTimeout + 2*c.StorageTimeout + c.CleanupGrace
	if _, err := New(fakeStore{}, CampaignProcessor{}, c); err == nil {
		t.Fatal("accepted exhausted lease budget")
	}
}

func TestScriptLimitDoesNotBecomeRetryableDuringCleanup(t *testing.T) {
	finished := false
	store := fakeStore{claim: func(context.Context, time.Duration) (jobs.Claim, error) { return sampleClaim(), nil },
		finish: func(_ context.Context, _ jobs.Claim, out jobs.Outcome) error {
			finished = true
			if out.FailureCode != jobs.ScriptLimitExceeded {
				t.Fatal("resource failure became retryable", out)
			}
			return nil
		}}
	processor := processorFunc(func(ctx context.Context, _ jobs.Job) (jobs.Outcome, error) {
		<-ctx.Done() // The independent cleanup phase used the remaining time.
		return jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ScriptLimitExceeded}, nil
	})
	config := ScriptConfig()
	config.ProcessingTimeout = time.Millisecond
	w, err := New(store, processor, config)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := w.RunOnce(context.Background()); err != nil || !worked || !finished {
		t.Fatal(worked, err, finished)
	}
}
