// Package worker orchestrates a claim, processing and fenced completion.
// It does not own the campaign rules or depend on a concrete SQL implementation.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"durable-jobs/internal/jobs"
)

type Store interface {
	RecoverExpired(context.Context, int) (int, error)
	Claim(context.Context, time.Duration) (jobs.Claim, error)
	Finish(context.Context, jobs.Claim, jobs.Outcome) error
}

// Processor must cooperate with cancellation and must not perform external
// side effects. A context deadline cannot forcibly stop arbitrary Go code.
// Versioned Python jobs execute only through the separate container runner.
type Processor interface {
	Process(context.Context, jobs.Job) (jobs.Outcome, error)
}

type Config struct {
	LeaseDuration     time.Duration
	ProcessingTimeout time.Duration
	StorageTimeout    time.Duration
	PollInterval      time.Duration
	CleanupGrace      time.Duration
}

func DefaultConfig() Config {
	return Config{LeaseDuration: 30 * time.Second, ProcessingTimeout: 10 * time.Second,
		StorageTimeout: 3 * time.Second, PollInterval: time.Second}
}

type Worker struct {
	store     Store
	processor Processor
	config    Config
	logger    *slog.Logger
}

func (w *Worker) SetLogger(logger *slog.Logger) {
	if logger != nil {
		w.logger = logger
	}
}

func New(store Store, processor Processor, config Config) (*Worker, error) {
	if store == nil || processor == nil {
		return nil, errors.New("worker requires a store and processor")
	}
	if jobs.ValidateLeaseDuration(config.LeaseDuration) != nil || config.ProcessingTimeout <= 0 ||
		config.StorageTimeout <= 0 || config.PollInterval <= 0 || config.CleanupGrace < 0 || config.CleanupGrace >= config.LeaseDuration ||
		config.ProcessingTimeout >= config.LeaseDuration || config.StorageTimeout >= config.LeaseDuration ||
		config.LeaseDuration <= config.ProcessingTimeout+2*config.StorageTimeout+config.CleanupGrace {
		return nil, errors.New("worker needs positive deadlines and a lease longer than processing, cleanup grace and two storage deadlines")
	}
	return &Worker{store: store, processor: processor, config: config, logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}, nil
}

// RunOnce recovers expired claims, then processes at most one due job. false,nil
// means no job was claimed; recovery may still have changed jobs and history.
// An error after claiming leaves an observable running job, not fake success.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	recoveryCtx, cancelRecovery := context.WithTimeout(ctx, w.config.StorageTimeout)
	_, err := w.store.RecoverExpired(recoveryCtx, 10)
	cancelRecovery()
	if err != nil {
		return false, fmt.Errorf("recover expired claims: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	claimCtx, cancelClaim := context.WithTimeout(ctx, w.config.StorageTimeout)
	claim, err := w.store.Claim(claimCtx, w.config.LeaseDuration)
	cancelClaim()
	if errors.Is(err, jobs.ErrNoQueuedJob) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	w.logger.Info("job claimed", "job_id", claim.Job.ID, "attempt", claim.Job.Attempts)
	processCtx, cancelProcess := context.WithTimeout(ctx, w.config.ProcessingTimeout)
	outcome, processErr := w.processor.Process(processCtx, claim.Job)
	deadlineErr := processCtx.Err()
	cancelProcess()
	if errors.Is(processErr, ErrStopWorker) {
		// Cleanup uncertainty is NOT a retryable timeout, even if cancellation
		// happened first. Best-effort fenced recording survives caller shutdown.
		finishCtx, cancelFinish := context.WithTimeout(context.Background(), w.config.StorageTimeout)
		defer cancelFinish()
		finishErr := w.store.Finish(finishCtx, claim, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.SandboxCleanupFailed})
		return true, errors.Join(ErrStopWorker, finishErr)
	}
	if err := ctx.Err(); err != nil {
		// Shutdown is not a business failure. Another poll recovers it after expiry.
		return true, err
	}
	// An observed script/resource failure with confirmed cleanup stays terminal
	// even when cleanup used the final milliseconds of the processing deadline.
	terminalScriptFailure := processErr == nil && outcome.Status == jobs.Failed &&
		(outcome.FailureCode == jobs.ScriptFailed || outcome.FailureCode == jobs.ScriptLimitExceeded || outcome.FailureCode == jobs.ScriptVersionUnavailable)
	if !terminalScriptFailure && (errors.Is(deadlineErr, context.DeadlineExceeded) || errors.Is(processErr, context.DeadlineExceeded)) {
		outcome = jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ProcessingTimeout}
	} else if errors.Is(processErr, jobs.ErrTransient) {
		outcome = jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.TransientFailure}
	} else if processErr != nil {
		outcome = jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ProcessingFailed}
	}
	if err := outcome.Validate(); err != nil {
		return true, err
	}
	finishCtx, cancelFinish := context.WithTimeout(ctx, w.config.StorageTimeout)
	defer cancelFinish()
	if err := w.store.Finish(finishCtx, claim, outcome); err != nil {
		w.logger.Error("job completion unconfirmed", "job_id", claim.Job.ID, "attempt", claim.Job.Attempts, "error", err)
		return true, fmt.Errorf("finish job %s: %w", claim.Job.ID, err)
	}
	w.logger.Info("job outcome committed", "job_id", claim.Job.ID, "attempt", claim.Job.Attempts, "attempt_outcome", outcome.Status, "failure_code", outcome.FailureCode)
	return true, nil
}

// Run polls without busy-spinning on empty queues or database failures. Each
// process runs one job at a time; separate worker processes may run concurrently.
func (w *Worker) Run(ctx context.Context, logger *slog.Logger) error {
	for {
		worked, err := w.RunOnce(ctx)
		if errors.Is(err, ErrStopWorker) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logger.Error("worker iteration failed", "error", err)
		} else if worked {
			continue
		}
		timer := time.NewTimer(w.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
