package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
)

const testJobID = "67b001de-ddb4-4c3d-98e4-2fc9466de033"

type fakeJobStore struct {
	createError  error
	getError     error
	createCalls  int
	getCalls     int
	lastKey      string
	historyError error
	historyCalls int
}

func (s *fakeJobStore) ListAttempts(context.Context, string) ([]jobs.Attempt, error) {
	s.historyCalls++
	return []jobs.Attempt{}, s.historyError
}

func (s *fakeJobStore) Create(_ context.Context, request jobs.SubmitRequest, key string) (jobs.Job, error) {
	s.createCalls++
	s.lastKey = key
	if s.createError != nil {
		return jobs.Job{}, s.createError
	}
	return jobs.Job{ID: testJobID, Type: request.Type, Payload: request.Payload, Status: "queued", CreatedAt: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}, nil
}

func (s *fakeJobStore) Get(_ context.Context, id string) (jobs.Job, error) {
	s.getCalls++
	if s.getError != nil {
		return jobs.Job{}, s.getError
	}
	return jobs.Job{ID: id, Type: "migrate_campaign", Payload: json.RawMessage("{}"), Status: "queued"}, nil
}

func testHandler(store *fakeJobStore) http.Handler {
	return NewHandler(fakeDatabase{}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSubmitJob(t *testing.T) {
	valid := "{\"type\":\"migrate_campaign\",\"payload\":{\"campaign_id\":\"campaign_42\"}}"
	tests := []struct {
		name        string
		contentType string
		body        string
		storeError  error
		wantStatus  int
		wantCalls   int
	}{
		{"accepted", "application/json", valid, nil, 202, 1},
		{"JSON charset", "application/json; charset=utf-8", valid, nil, 202, 1},
		{"missing content type", "", valid, nil, 415, 0},
		{"wrong content type", "text/plain", valid, nil, 415, 0},
		{"malformed JSON", "application/json", "{", nil, 400, 0},
		{"unsupported type", "application/json", "{\"type\":\"email\",\"payload\":{}}", nil, 400, 0},
		{"oversize request", "application/json", strings.Repeat(" ", int(MaxSubmissionBytes)+1), nil, 413, 0},
		{"exact byte limit", "application/json", valid + strings.Repeat(" ", int(MaxSubmissionBytes)-len(valid)), nil, 202, 1},
		{"database failure", "application/json", valid, errors.New("private database error"), 503, 1},
		{"commit timeout", "application/json", valid, context.DeadlineExceeded, 503, 1},
		{"JSONB rejection", "application/json", valid, jobs.ErrInvalidPayload, 400, 1},
		{"idempotency conflict", "application/json", valid, jobs.ErrIdempotencyConflict, 409, 1},
		{"invalid stored key", "application/json", valid, jobs.ErrInvalidIdempotencyKey, 400, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeJobStore{createError: tt.storeError}
			request := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", tt.contentType)
			response := httptest.NewRecorder()
			testHandler(store).ServeHTTP(response, request)
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if store.createCalls != tt.wantCalls {
				t.Fatalf("store called %d times, want %d", store.createCalls, tt.wantCalls)
			}
			if strings.Contains(response.Body.String(), "private database error") {
				t.Fatal("database error leaked")
			}
			if tt.wantStatus == 202 {
				if response.Header().Get("Location") != "/jobs/"+testJobID {
					t.Fatal("missing Location")
				}
				var job jobs.Job
				if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
					t.Fatal(err)
				}
				if job.ID != testJobID || job.Status != "queued" || job.CreatedAt.IsZero() {
					t.Fatalf("incomplete job: %+v", job)
				}
			} else if response.Header().Get("Location") != "" {
				t.Fatal("failure must not contain a successful job Location")
			}
		})
	}
}

func TestGetJob(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		storeError error
		wantStatus int
		wantCalls  int
	}{
		{"found", testJobID, nil, 200, 1},
		{"invalid id", "not-a-uuid", nil, 400, 0},
		{"missing job", testJobID, jobs.ErrNotFound, 404, 1},
		{"database failure", testJobID, errors.New("private database error"), 503, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeJobStore{getError: tt.storeError}
			response := httptest.NewRecorder()
			testHandler(store).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/jobs/"+tt.id, nil))
			if response.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if store.getCalls != tt.wantCalls {
				t.Fatalf("store called %d times, want %d", store.getCalls, tt.wantCalls)
			}
			if strings.Contains(response.Body.String(), "private database error") {
				t.Fatal("database error leaked")
			}
		})
	}
}
