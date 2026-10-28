package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/telemetry"
)

type metricsStore struct {
	fakeJobStore
	err       error
	malformed bool
}

func (s metricsStore) Metrics(ctx context.Context) (telemetry.Snapshot, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 2*time.Second {
		panic("missing bounded metric query")
	}
	value := telemetry.Snapshot{Queued: 3, StartBuckets: make([]int64, len(telemetry.Buckets)), CompletionBuckets: make([]int64, len(telemetry.Buckets))}
	if s.malformed {
		value.StartBuckets = nil
	}
	return value, s.err
}

func TestMetricsEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name  string
		store JobStore
		code  int
	}{
		{"available", &metricsStore{}, 200},
		{"unavailable", nil, 503},
		{"database error", &metricsStore{err: errors.New("secret connection string")}, 503},
		{"incomplete histogram", &metricsStore{malformed: true}, 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			NewHandler(fakeDatabase{}, tt.store, slog.New(slog.NewJSONHandler(io.Discard, nil))).ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
			if r.Code != tt.code || strings.Contains(r.Body.String(), "secret") {
				t.Fatalf("response: %d %s", r.Code, r.Body.String())
			}
			if tt.code == 200 && (!strings.Contains(r.Header().Get("Content-Type"), "version=0.0.4") || !strings.Contains(r.Body.String(), "durable_jobs_queue_depth 3")) {
				t.Fatal("invalid Prometheus response")
			}
		})
	}
}
