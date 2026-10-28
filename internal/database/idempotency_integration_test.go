package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/httpapi"
	"durable-jobs/internal/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
)

func submission(payload string) jobs.SubmitRequest {
	return jobs.SubmitRequest{Type: "migrate_campaign", Payload: json.RawMessage(payload)}
}

func TestIdempotencyPayloadEqualityIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	original := `{"campaign_id":"synthetic_42","n":1,"nested":{"a":true,"b":null},"steps":[1,2]}`
	first, err := store.Create(ctx, submission(original), "payload-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		original,
		`{ "steps": [1,2], "nested":{"b":null,"a":true}, "n":1.0, "campaign_id":"synthetic_42" }`,
	} {
		replay, err := store.Create(ctx, submission(payload), "payload-key")
		if err != nil || replay.ID != first.ID || !replay.CreatedAt.Equal(first.CreatedAt) || string(replay.Payload) != string(first.Payload) {
			t.Fatalf("replay=%+v error=%v", replay, err)
		}
	}
	for _, payload := range []string{
		`{"campaign_id":"different"}`,
		`{"campaign_id":"synthetic_42","n":1,"nested":{"a":true,"b":null},"steps":[2,1]}`,
		`{"campaign_id":"synthetic_42","n":"1","nested":{"a":true,"b":null},"steps":[1,2]}`,
	} {
		_, err := store.Create(ctx, submission(payload), "payload-key")
		if !errors.Is(err, jobs.ErrIdempotencyConflict) {
			t.Fatalf("conflict error=%v", err)
		}
	}
	// Compare in PostgreSQL, never through float64: these integers differ by one.
	_, err = store.Create(ctx, submission(`{"n":9007199254740992}`), "large-number")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(ctx, submission(`{"n":9007199254740993}`), "large-number")
	if !errors.Is(err, jobs.ErrIdempotencyConflict) {
		t.Fatalf("lost numeric precision: %v", err)
	}
	for _, key := range []string{"different-key", "PAYLOAD-KEY", ""} {
		created, err := store.Create(ctx, submission(original), key)
		if err != nil || created.ID == first.ID {
			t.Fatalf("new key=%q job=%+v error=%v", key, created, err)
		}
	}
	assertJobCount(t, pool, 5)
}

func TestConcurrentIdempotentHTTPSubmissionsIntegration(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_input=%t", conflicting), func(t *testing.T) {
			pool, config := isolatedDatabase(t)
			secondPool, err := pgxpool.NewWithConfig(context.Background(), config.Copy())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(secondPool.Close)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			// Independent API instances and pools: no shared in-memory lock/cache.
			handlers := []http.Handler{
				httpapi.NewHandler(pool, NewJobStore(pool), logger),
				httpapi.NewHandler(secondPool, NewJobStore(secondPool), logger),
			}
			const attempts = 16
			start := make(chan struct{})
			responses := make(chan *httptest.ResponseRecorder, attempts)
			for i := 0; i < attempts; i++ {
				go func(i int) {
					<-start
					variant := 0
					if conflicting {
						variant = i % 2
					}
					body := fmt.Sprintf(`{"type":"migrate_campaign","payload":{"variant":%d}}`, variant)
					responses <- postJob(handlers[i%len(handlers)], body, "concurrent-key")
				}(i)
			}
			close(start)
			accepted, conflicts := 0, 0
			var id string
			for i := 0; i < attempts; i++ {
				response := <-responses
				switch response.Code {
				case http.StatusAccepted:
					accepted++
					var job jobs.Job
					if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
						t.Fatal(err)
					}
					if id == "" {
						id = job.ID
					}
					if job.ID != id || response.Header().Get("Location") != "/jobs/"+id {
						t.Fatalf("different job returned: %s != %s", job.ID, id)
					}
				case http.StatusConflict:
					conflicts++
					if response.Header().Get("Location") != "" {
						t.Fatal("conflict returned Location")
					}
				default:
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
			wantAccepted, wantConflicts := attempts, 0
			if conflicting {
				wantAccepted, wantConflicts = attempts/2, attempts/2
			}
			if accepted != wantAccepted || conflicts != wantConflicts {
				t.Fatalf("accepted=%d conflicts=%d", accepted, conflicts)
			}
			assertJobCount(t, pool, 1)
		})
	}
}

func TestIdempotencySurvivesRestartIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := `{"type":"migrate_campaign","payload":{"campaign_id":"synthetic_restart_retry"}}`
	first := postJob(httpapi.NewHandler(pool, NewJobStore(pool), logger), body, "restart-key")
	if first.Code != 202 {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	pool.Close()
	reopened, err := pgxpool.NewWithConfig(context.Background(), config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	replay := postJob(httpapi.NewHandler(reopened, NewJobStore(reopened), logger), body, "restart-key")
	if replay.Code != 202 || replay.Body.String() != first.Body.String() || replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("restart replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	assertJobCount(t, reopened, 1)
}

func TestFailedSubmissionsDoNotReserveKeyIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE jobs ADD CONSTRAINT test_unique_payload
		UNIQUE (payload) DEFERRABLE INITIALLY DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, submission(`{"n":1}`), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, submission(`{"n":1}`), "commit-failed"); err == nil {
		t.Fatal("expected COMMIT failure")
	}
	if _, err := store.Create(ctx, submission(`{"n":2}`), "commit-failed"); err != nil {
		t.Fatalf("failed commit reserved key: %v", err)
	}
	if _, err := store.Create(ctx, submission(`{"text":"\u0000"}`), "invalid-jsonb"); !errors.Is(err, jobs.ErrInvalidPayload) {
		t.Fatalf("error=%v", err)
	}
	if _, err := store.Create(ctx, submission(`{"n":3}`), "invalid-jsonb"); err != nil {
		t.Fatalf("invalid payload reserved key: %v", err)
	}
	if _, err := store.Create(ctx, submission(`{}`), "bad key"); !errors.Is(err, jobs.ErrInvalidIdempotencyKey) {
		t.Fatalf("error=%v", err)
	}
	// The database independently protects callers that bypass key validation.
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (type, payload, idempotency_key)
		VALUES ('migrate_campaign', '{}', 'bad key')`); err == nil {
		t.Fatal("database accepted invalid key")
	}
	assertJobCount(t, pool, 3)
}

func TestIdempotencyWaitsForTransactionOutcomeIntegration(t *testing.T) {
	for _, outcome := range []string{"commit", "rollback", "cancel_waiter"} {
		t.Run(outcome, func(t *testing.T) {
			pool, config := isolatedDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(owner)
			var ownerID string
			if err := owner.QueryRow(ctx, `INSERT INTO jobs (type, payload, idempotency_key)
				VALUES ('migrate_campaign', '{"n":1}', 'held-key') RETURNING id::text`).Scan(&ownerID); err != nil {
				t.Fatal(err)
			}
			config.MaxConns = 1
			// Explicit transaction isolation must work even if the session default differs.
			config.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
			waiterPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
			if err != nil {
				t.Fatal(err)
			}
			defer waiterPool.Close()
			var waiterPID int
			if err := waiterPool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&waiterPID); err != nil {
				t.Fatal(err)
			}
			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()
			type result struct {
				job jobs.Job
				err error
			}
			done := make(chan result, 1)
			go func() {
				job, err := NewJobStore(waiterPool).Create(waitCtx, submission(`{"n":1}`), "held-key")
				done <- result{job, err}
			}()
			// Observe the lock wait, not an arbitrary sleep: INSERT is definitely
			// overlapping the owner's uncommitted key before we release it.
			waitForLock(t, ctx, pool, waiterPID)
			if outcome == "cancel_waiter" {
				cancelWait()
				if result := <-done; result.err == nil {
					t.Fatal("canceled waiter returned success")
				}
			}
			if outcome == "rollback" {
				err = owner.Rollback(ctx)
			} else {
				err = owner.Commit(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			var got result
			if outcome == "cancel_waiter" {
				got.job, got.err = NewJobStore(waiterPool).Create(ctx, submission(`{"n":1}`), "held-key")
			} else {
				got = <-done
			}
			if got.err != nil {
				t.Fatal(got.err)
			}
			if outcome == "rollback" && got.job.ID == ownerID {
				t.Fatal("returned rolled-back job")
			}
			if outcome != "rollback" && got.job.ID != ownerID {
				t.Fatal("did not return committed job")
			}
			assertJobCount(t, pool, 1)
		})
	}
}

func waitForLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT COALESCE(bool_or(wait_event_type = 'Lock'), false)
			FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatal("submission never waited for the concurrent transaction")
		case <-ticker.C:
		}
	}
}

func postJob(handler http.Handler, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "/jobs", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertJobCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("jobs=%d want=%d", count, want)
	}
}
