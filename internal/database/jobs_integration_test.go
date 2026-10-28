package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/httpapi"
	"durable-jobs/internal/jobs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every integration test owns a new randomly named schema. Cleanup only drops
// that schema; existing application jobs and the Docker volume are untouched.
func isolatedDatabase(t *testing.T) (*pgxpool.Pool, *pgxpool.Config) {
	t.Helper()
	pool, config := unmigratedIsolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool, config
}

func unmigratedIsolatedDatabase(t *testing.T) (*pgxpool.Pool, *pgxpool.Config) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("real database tests: run make integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "test_jobs_" + hex.EncodeToString(random[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("clean up owned test schema %s: %v", schema, err)
		}
	})
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 4
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, config
}

func TestJobStorageIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
	var migrationCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 6 {
		t.Fatalf("migrations = %d, want 6", migrationCount)
	}

	store := NewJobStore(pool)
	request := jobs.SubmitRequest{Type: "migrate_campaign", Payload: json.RawMessage("{\"campaign_id\":\"synthetic_storage_test\"}")}
	created, err := store.Create(ctx, request, "")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Status != "queued" || created.CreatedAt.IsZero() {
		t.Fatalf("incomplete job: %+v", created)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.Type != request.Type || got.Status != "queued" || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	var payload map[string]string
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["campaign_id"] != "synthetic_storage_test" {
		t.Fatalf("payload = %s", got.Payload)
	}

	// Without a key, repeated submissions still create distinct jobs.
	second, err := store.Create(ctx, request, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == created.ID {
		t.Fatal("distinct submissions unexpectedly reused an ID")
	}

	_, err = store.Get(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("missing job error = %v", err)
	}

	_, err = store.Create(ctx, jobs.SubmitRequest{Type: "unsupported", Payload: json.RawMessage("{}")}, "")
	if err == nil {
		t.Fatal("database constraint should reject invalid job type")
	}

	_, err = store.Create(ctx, jobs.SubmitRequest{Type: "migrate_campaign", Payload: json.RawMessage("{\"text\":\"\\u0000\"}")}, "")
	if !errors.Is(err, jobs.ErrInvalidPayload) {
		t.Fatalf("JSONB rejection = %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("failed transactions left rows: count = %d", count)
	}
}

func TestHTTPPersistenceAcrossServerRestartIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(httpapi.NewHandler(pool, NewJobStore(pool), logger))
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(server.URL+"/jobs", "application/json", strings.NewReader("{\"type\":\"migrate_campaign\",\"payload\":{\"campaign_id\":\"synthetic_restart_test\"}}"))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	var created jobs.Job
	decodeErr := json.NewDecoder(response.Body).Decode(&created)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || decodeErr != nil {
		server.Close()
		t.Fatalf("submit status=%d error=%v", response.StatusCode, decodeErr)
	}
	location := response.Header.Get("Location")
	server.Close()
	pool.Close()

	// A new HTTP server AND connection pool cannot rely on in-memory job state.
	reopened, err := pgxpool.NewWithConfig(context.Background(), config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	restarted := httptest.NewServer(httpapi.NewHandler(reopened, NewJobStore(reopened), logger))
	t.Cleanup(restarted.Close)
	response, err = client.Get(restarted.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var retrieved jobs.Job
	if err := json.NewDecoder(response.Body).Decode(&retrieved); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || retrieved.ID != created.ID || retrieved.Status != "queued" {
		t.Fatalf("job did not survive restart: status=%d job=%+v", response.StatusCode, retrieved)
	}
}

func TestCommitFailureIsNotAcknowledgedIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Test-only constraint: the duplicate passes INSERT ... RETURNING, but fails
	// at COMMIT. This constraint is never installed in the application schema.
	if _, err := pool.Exec(ctx, "ALTER TABLE jobs ADD CONSTRAINT test_unique_payload UNIQUE (payload) DEFERRABLE INITIALLY DEFERRED"); err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewHandler(pool, NewJobStore(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i, want := range []int{http.StatusAccepted, http.StatusServiceUnavailable} {
		request := httptest.NewRequest("POST", "/jobs", strings.NewReader("{\"type\":\"migrate_campaign\",\"payload\":{\"campaign_id\":\"synthetic_commit_test\"}}"))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("attempt %d status=%d want=%d body=%s", i, response.Code, want, response.Body.String())
		}
		if want != 202 && response.Header().Get("Location") != "" {
			t.Fatal("failed commit was acknowledged with a Location")
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("committed jobs = %d, want 1", count)
	}
}
