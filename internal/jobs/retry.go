package jobs

import "time"

const DefaultMaxAttempts = 3

// AfterFailure is a pure domain policy. Unknown failures are terminal by default.
// Limits are stored per job; claim attempts (including crashes) consume the budget.
// Backoff is deterministic for this local demo; fleet-scale jitter is not included.
func AfterFailure(code string, attempts, maxAttempts int) (State, time.Duration) {
	if attempts < 1 || maxAttempts < 1 || maxAttempts > 10 || attempts >= maxAttempts {
		return Failed, 0
	}
	switch code {
	case ProcessingTimeout, TransientFailure, LeaseExpired:
		delay := time.Second
		for i := 1; i < attempts && delay < time.Minute; i++ {
			delay *= 2
		}
		if delay > time.Minute {
			delay = time.Minute
		}
		return RetryWaiting, delay
	default:
		return Failed, 0
	}
}

// Attempt is a public audit record. It deliberately contains no lease token.
// LeaseExpiresAt can be nil for terminal attempts imported from schema 003,
// which did not retain the original expiry after completion.
type Attempt struct {
	Number         int        `json:"number"`
	Status         string     `json:"status"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	FailureCode    string     `json:"failure_code,omitempty"`
}
