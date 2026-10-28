package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubmissionIdempotencyHeaders(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values []string
		status int
		calls  int
	}{
		{"absent remains supported", nil, 202, 1},
		{"valid key", []string{"Campaign_42:v1"}, 202, 1},
		{"empty header", []string{""}, 400, 0},
		{"repeated headers", []string{"key", "key"}, 400, 0},
		{"different headers", []string{"one", "two"}, 400, 0},
		{"comma joined", []string{"one,two"}, 400, 0},
		{"oversize key", []string{strings.Repeat("x", 129)}, 400, 0},
		{"whitespace", []string{"one two"}, 400, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeJobStore{}
			request := httptest.NewRequest("POST", "/jobs", strings.NewReader(`{"type":"migrate_campaign","payload":{}}`))
			request.Header.Set("Content-Type", "application/json")
			for _, value := range tt.values {
				request.Header.Add("Idempotency-Key", value)
			}
			response := httptest.NewRecorder()
			testHandler(store).ServeHTTP(response, request)
			if response.Code != tt.status || store.createCalls != tt.calls {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, store.createCalls, response.Body.String())
			}
			if tt.calls == 1 && len(tt.values) == 1 && store.lastKey != tt.values[0] {
				t.Fatalf("key changed: got %q", store.lastKey)
			}
			if tt.calls == 0 && response.Header().Get("Location") != "" {
				t.Fatal("invalid key must not return a job location")
			}
		})
	}
}

func TestSubmissionTimeoutRetryGuidance(t *testing.T) {
	for _, key := range []string{"", "retry-key"} {
		store := &fakeJobStore{createError: context.DeadlineExceeded}
		request := httptest.NewRequest("POST", "/jobs", strings.NewReader(`{"type":"migrate_campaign","payload":{}}`))
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		testHandler(store).ServeHTTP(response, request)
		want := "without an Idempotency-Key a retry can create a duplicate"
		if key != "" {
			want = "retry with the same Idempotency-Key and submission"
		}
		if response.Code != 503 || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("key=%q status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
}
