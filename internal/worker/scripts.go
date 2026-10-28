package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/sandbox"
	"durable-jobs/internal/scripts"
)

// ErrStopWorker means continuing to claim jobs is unsafe. Run must return it,
// even during shutdown or if recording the terminal outcome also fails.
var ErrStopWorker = errors.New("sandbox cleanup unconfirmed; stop worker and inspect owned containers before restarting")

type ScriptRunner interface {
	ImageID() string
	Run(context.Context, sandbox.Request) sandbox.Result
}

type RunnerFactory func(context.Context) (ScriptRunner, error)

type VersionedProcessor struct{ NewRunner RunnerFactory }

func NewVersionedProcessor() VersionedProcessor {
	return VersionedProcessor{NewRunner: func(ctx context.Context) (ScriptRunner, error) { return sandbox.New(ctx) }}
}

// The process deadline bounds preflight/create/execute/inspect together.
// Cleanup can outlive it by six seconds; the lease reserves that explicitly.
func ScriptConfig() Config {
	c := DefaultConfig()
	c.LeaseDuration, c.ProcessingTimeout, c.CleanupGrace = time.Minute, 30*time.Second, sandbox.CleanupGrace
	return c
}

func (p VersionedProcessor) Process(ctx context.Context, job jobs.Job) (jobs.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return jobs.Outcome{}, err
	}
	if job.Type != "run_script" {
		return (CampaignProcessor{}).Process(ctx, job)
	}
	fail := func(code string) (jobs.Outcome, error) {
		return jobs.Outcome{Status: jobs.Failed, FailureCode: code}, nil
	}
	payload, err := scripts.Parse(job.Payload)
	if err != nil {
		return fail(jobs.ScriptVersionUnavailable)
	}
	source, err := scripts.Resolve(payload)
	if err != nil {
		return fail(jobs.ScriptVersionUnavailable)
	}
	if p.NewRunner == nil {
		return jobs.Outcome{}, jobs.ErrTransient
	}
	runner, err := p.NewRunner(ctx)
	if err != nil || runner == nil {
		return jobs.Outcome{}, jobs.ErrTransient
	}
	// Never resolve a client-provided tag/path or run a different local image.
	if runner.ImageID() != payload.ImageID {
		return fail(jobs.ScriptVersionUnavailable)
	}
	result := runner.Run(ctx, sandbox.Request{Script: source, Input: payload.Input})
	if !result.CleanupConfirmed || result.FailureCode == "cleanup_failed" {
		return jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.SandboxCleanupFailed}, ErrStopWorker
	}
	if result.Status == "succeeded" {
		if result.ImageID != payload.ImageID || result.ScriptSHA256 != payload.SourceSHA256 || result.FailureCode != "" {
			return fail(jobs.ScriptVersionUnavailable)
		}
		if (jobs.Outcome{Status: jobs.Succeeded, Result: result.Output}).Validate() != nil {
			return fail(jobs.ScriptFailed)
		}
		// Store bounded output AND execution identity together under the lease.
		encoded, err := json.Marshal(struct {
			Output           json.RawMessage `json:"output"`
			Script           string          `json:"script"`
			Version          string          `json:"version"`
			SourceSHA256     string          `json:"source_sha256"`
			ImageID          string          `json:"image_id"`
			CleanupConfirmed bool            `json:"cleanup_confirmed"`
		}{result.Output, payload.Script, payload.Version, payload.SourceSHA256, payload.ImageID, true})
		if err != nil {
			return fail(jobs.ScriptFailed)
		}
		if len(encoded) > jobs.MaxResultBytes {
			return fail(jobs.ScriptLimitExceeded)
		}
		return jobs.Outcome{Status: jobs.Succeeded, Result: encoded}, nil
	}
	switch result.FailureCode {
	case "time_limit", "memory_limit", "output_limit":
		return fail(jobs.ScriptLimitExceeded)
	case "runner_unavailable", "container_create_failed", "container_state_unknown", "container_attach_failed":
		return jobs.Outcome{}, jobs.ErrTransient
	case "canceled":
		if ctx.Err() != nil {
			return jobs.Outcome{}, ctx.Err()
		}
	}
	return fail(jobs.ScriptFailed)
}
