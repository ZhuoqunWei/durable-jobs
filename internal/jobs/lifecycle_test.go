package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLifecycleTransitions(t *testing.T) {
	states := []State{Queued, RetryWaiting, Running, Succeeded, Failed, "unknown"}
	for _, from := range states {
		for _, to := range states {
			want := ((from == Queued || from == RetryWaiting) && to == Running) ||
				(from == Running && (to == Succeeded || to == Failed || to == RetryWaiting))
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestOutcomeValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome Outcome
		valid   bool
	}{
		{"success", Outcome{Status: Succeeded, Result: json.RawMessage(`{"ok":true}`)}, true},
		{"failure", Outcome{Status: Failed, FailureCode: InvalidCampaign}, true},
		{"timeout", Outcome{Status: Failed, FailureCode: ProcessingTimeout}, true},
		{"unsupported", Outcome{Status: Failed, FailureCode: UnsupportedJobType}, true},
		{"processing failure", Outcome{Status: Failed, FailureCode: ProcessingFailed}, true},
		{"transient failure", Outcome{Status: Failed, FailureCode: TransientFailure}, true},
		{"processor cannot schedule retries", Outcome{Status: RetryWaiting, FailureCode: TransientFailure}, false},
		{"processor cannot expire a lease", Outcome{Status: Failed, FailureCode: LeaseExpired}, false},
		{"missing result", Outcome{Status: Succeeded}, false},
		{"array", Outcome{Status: Succeeded, Result: json.RawMessage(`[]`)}, false},
		{"null", Outcome{Status: Succeeded, Result: json.RawMessage(`null`)}, false},
		{"invalid JSON", Outcome{Status: Succeeded, Result: json.RawMessage(`{`)}, false},
		{"invalid UTF8", Outcome{Status: Succeeded, Result: []byte{'{', '"', 'a', '"', ':', '"', 0xff, '"', '}'}}, false},
		{"oversize", Outcome{Status: Succeeded, Result: json.RawMessage(`{"x":"` + strings.Repeat("x", MaxResultBytes) + `"}`)}, false},
		{"success with failure", Outcome{Status: Succeeded, Result: json.RawMessage(`{}`), FailureCode: InvalidCampaign}, false},
		{"failure with result", Outcome{Status: Failed, Result: json.RawMessage(`{}`), FailureCode: InvalidCampaign}, false},
		{"raw error forbidden", Outcome{Status: Failed, FailureCode: "private database credential"}, false},
		{"missing failure", Outcome{Status: Failed}, false},
		{"not terminal", Outcome{Status: Running}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.outcome.Validate()
			if (err == nil) != tc.valid || (err != nil && !errors.Is(err, ErrInvalidOutcome)) {
				t.Fatalf("Validate() = %v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestLeaseBounds(t *testing.T) {
	for _, duration := range []time.Duration{-1, 0, time.Microsecond, 5*time.Minute + 1} {
		if !errors.Is(ValidateLeaseDuration(duration), ErrInvalidLease) {
			t.Errorf("accepted %s", duration)
		}
	}
	for _, duration := range []time.Duration{time.Millisecond, 30 * time.Second, 5 * time.Minute} {
		if err := ValidateLeaseDuration(duration); err != nil {
			t.Errorf("%s: %v", duration, err)
		}
	}
}
