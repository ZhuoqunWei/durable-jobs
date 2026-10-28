package httpapi

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"durable-jobs/internal/jobs"
)

func TestAttemptHistoryEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, id      string
		err           error
		status, calls int
	}{
		{"empty", testJobID, nil, 200, 1}, {"invalid", "bad-id", nil, 400, 0},
		{"missing", testJobID, jobs.ErrNotFound, 404, 1}, {"unavailable", testJobID, errors.New("private database credentials"), 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeJobStore{historyError: tc.err}
			response := httptest.NewRecorder()
			testHandler(store).ServeHTTP(response, httptest.NewRequest("GET", "/jobs/"+tc.id+"/attempts", nil))
			if response.Code != tc.status || store.historyCalls != tc.calls {
				t.Fatalf("%d calls=%d %s", response.Code, store.historyCalls, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private") {
				t.Fatal("private error leaked")
			}
			if response.Code == 200 {
				var body struct {
					JobID    string         `json:"job_id"`
					Attempts []jobs.Attempt `json:"attempts"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.JobID != testJobID || body.Attempts == nil || len(body.Attempts) != 0 {
					t.Fatalf("body=%s err=%v", response.Body.String(), err)
				}
			}
		})
	}
}
