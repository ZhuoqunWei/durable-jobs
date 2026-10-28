package jobs

import (
	"testing"
	"time"
)

func TestRetryPolicy(t *testing.T) {
	for _, code := range []string{TransientFailure, ProcessingTimeout, LeaseExpired} {
		for _, tc := range []struct {
			attempt, max int
			state        State
			delay        time.Duration
		}{
			{1, 3, RetryWaiting, time.Second}, {2, 3, RetryWaiting, 2 * time.Second}, {3, 3, Failed, 0},
			{1, 1, Failed, 0}, {0, 3, Failed, 0}, {1, 0, Failed, 0}, {1, 11, Failed, 0}, {7, 10, RetryWaiting, time.Minute}, {9, 10, RetryWaiting, time.Minute},
		} {
			state, delay := AfterFailure(code, tc.attempt, tc.max)
			if state != tc.state || delay != tc.delay {
				t.Errorf("%s attempt=%d max=%d: %s %s", code, tc.attempt, tc.max, state, delay)
			}
		}
	}
	for _, code := range []string{InvalidCampaign, UnsupportedJobType, ProcessingFailed, "unknown", ""} {
		if state, delay := AfterFailure(code, 1, 3); state != Failed || delay != 0 {
			t.Errorf("retried permanent/unknown error %q", code)
		}
	}
}
