package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

type State string

const (
	Queued       State = "queued"
	RetryWaiting State = "retry_waiting"
	Running      State = "running"
	Succeeded    State = "succeeded"
	Failed       State = "failed"
)

// CanTransitionTo describes the job lifecycle. The SQL store
// enforces these transitions atomically; checking a Go value alone cannot do so.
func (s State) CanTransitionTo(next State) bool {
	return ((s == Queued || s == RetryWaiting) && next == Running) ||
		(s == Running && (next == Succeeded || next == Failed || next == RetryWaiting))
}

var (
	ErrNoQueuedJob    = errors.New("no queued job is available")
	ErrLeaseLost      = errors.New("job claim is missing, expired, or no longer current")
	ErrInvalidOutcome = errors.New("invalid processing outcome")
	ErrInvalidLease   = errors.New("lease duration must be between 1 millisecond and 5 minutes")
	ErrTransient      = errors.New("temporary processing failure")
)

const (
	InvalidCampaign          = "invalid_campaign"
	UnsupportedJobType       = "unsupported_job_type"
	ProcessingTimeout        = "processing_timeout"
	ProcessingFailed         = "processing_failed"
	TransientFailure         = "transient_failure"
	LeaseExpired             = "lease_expired"
	ScriptFailed             = "script_failed"
	ScriptLimitExceeded      = "script_limit_exceeded"
	ScriptVersionUnavailable = "script_version_unavailable"
	SandboxCleanupFailed     = "sandbox_cleanup_failed"
	MaxResultBytes           = 64 << 10
)

// Claim is internal worker authority, not an HTTP response. Never log its token.
type Claim struct {
	Job   Job
	Token string
}

// Outcome holds either a JSON result or a stable, non-sensitive failure code.
// Arbitrary processor errors are never persisted as public error messages.
type Outcome struct {
	Status      State
	Result      json.RawMessage
	FailureCode string
}

func (o Outcome) Validate() error {
	// A processor returns an outcome; only the retry policy may schedule a retry.
	if o.Status != Succeeded && o.Status != Failed {
		return ErrInvalidOutcome
	}
	if o.Status == Succeeded {
		result := bytes.TrimSpace(o.Result)
		if o.FailureCode != "" || len(result) == 0 || len(o.Result) > MaxResultBytes || result[0] != '{' || !utf8.Valid(result) || !json.Valid(result) {
			return ErrInvalidOutcome
		}
		return nil
	}
	if len(o.Result) != 0 {
		return ErrInvalidOutcome
	}
	switch o.FailureCode {
	case InvalidCampaign, UnsupportedJobType, ProcessingTimeout, ProcessingFailed, TransientFailure,
		ScriptFailed, ScriptLimitExceeded, ScriptVersionUnavailable, SandboxCleanupFailed:
		return nil
	default:
		return ErrInvalidOutcome
	}
}

func ValidateLeaseDuration(duration time.Duration) error {
	if duration < time.Millisecond || duration > 5*time.Minute {
		return ErrInvalidLease
	}
	return nil
}
