package database

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/httpapi"
	"durable-jobs/internal/jobs"
	"durable-jobs/internal/sandbox"
	"durable-jobs/internal/scripts"
	"durable-jobs/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

const scriptTestImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestVersionedJobJSONBSizeIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	// <64 KiB compact on the wire; >64 KiB with JSONB's added spaces.
	input := []byte(`{"values":[` + strings.Repeat("0,", 27999) + `0]}`)
	job, err := store.Create(ctx, scriptSubmission(input, scriptTestImage), "large-compact-input")
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Payload) <= 64<<10 {
		t.Fatal("test did not exercise JSONB formatting expansion")
	}
	parsed, err := scripts.Parse(job.Payload)
	if err != nil || !bytes.Equal(parsed.Input, input) {
		t.Fatal("formatting changed valid input", err)
	}
	// Unlike whitespace, exponent expansion cannot be compacted away. Reject
	// before COMMIT so the worker never receives an oversized canonical input.
	if _, err := store.Create(ctx, scriptSubmission([]byte(`{"n":1e70000}`), scriptTestImage), "expanded-number"); !errors.Is(err, jobs.ErrInvalidPayload) {
		t.Fatal("oversized JSONB number accepted", err)
	}
	assertJobCount(t, pool, 1)
}

func scriptSubmission(input json.RawMessage, image string) jobs.SubmitRequest {
	payload, _ := json.Marshal(scripts.Payload{Script: scripts.CampaignName, Version: scripts.CampaignVersion,
		SourceSHA256: scripts.CampaignSHA256, ImageID: image, Input: input})
	return jobs.SubmitRequest{Type: "run_script", Payload: payload}
}

// No Docker required: prove every new failure code is accepted by the SQL
// constraints, stored atomically in history, and cannot become retry_waiting.
func TestVersionedJobFailuresIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	for _, code := range []string{jobs.ScriptFailed, jobs.ScriptLimitExceeded, jobs.ScriptVersionUnavailable, jobs.SandboxCleanupFailed} {
		job, err := store.Create(ctx, scriptSubmission([]byte(validCampaign), scriptTestImage), "")
		if err != nil {
			t.Fatal(err)
		}
		claim, err := store.Claim(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Failed, FailureCode: code}); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, job.ID)
		if err != nil || got.Status != jobs.Failed || got.FailureCode != code || got.Attempts != 1 {
			t.Fatal(got, err)
		}
		history, err := store.ListAttempts(ctx, job.ID)
		if err != nil || len(history) != 1 || history[0].FailureCode != code || history[0].Status != "failed" {
			t.Fatal(history, err)
		}
	}
	if _, err := store.Claim(ctx, time.Minute); !errors.Is(err, jobs.ErrNoQueuedJob) {
		t.Fatal("terminal script retried", err)
	}
	valid := scriptSubmission([]byte(validCampaign), scriptTestImage)
	for _, bad := range []string{`{}`, strings.Replace(string(valid.Payload), `"input":`+validCampaign, `"input":null`, 1), strings.Replace(string(valid.Payload), scriptTestImage, "mutable:tag", 1), string(valid.Payload[:len(valid.Payload)-1]) + `,"source":"code"}`} {
		if _, err := store.Create(ctx, jobs.SubmitRequest{Type: "run_script", Payload: []byte(bad)}, ""); err == nil {
			t.Fatal("database accepted malformed script shape", bad)
		}
	}
}

func TestScriptQueueHTTPWorkerIntegration(t *testing.T) {
	if os.Getenv("SANDBOX_INTEGRATION") != "1" {
		t.Skip("real queue + Docker: make script-queue-check")
	}
	pool, config := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner, err := sandbox.New(ctx)
	if err != nil {
		t.Fatal("runner setup", err)
	}
	store := NewJobStore(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(httpapi.NewHandler(pool, store, logger))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	post := func(body []byte, key string) (int, jobs.Job) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/jobs", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var job jobs.Job
		if response.StatusCode == http.StatusAccepted {
			if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode, job
	}
	get := func(path string, output any) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		if err != nil || response.StatusCode != 200 || bytes.Contains(data, []byte("lease_token")) || json.Unmarshal(data, output) != nil {
			t.Fatalf("GET failed: %d %s %v", response.StatusCode, data, err)
		}
	}
	// Compile and restart the real production worker; it sees only this owned schema.
	binary := filepath.Join(t.TempDir(), "worker")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/worker").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	environment := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "DATABASE_URL=") && !strings.HasPrefix(value, "PGOPTIONS=") {
			environment = append(environment, value)
		}
	}
	schema := config.ConnConfig.RuntimeParams["search_path"]
	if !strings.HasPrefix(schema, "test_jobs_") {
		t.Fatal("refusing non-test schema")
	}
	environment = append(environment, "DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"), "PGOPTIONS=-c search_path="+schema)
	runWorker := func() {
		t.Helper()
		command := exec.CommandContext(ctx, binary, "-once")
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("worker failed: %v %s", err, output)
		}
	}
	validBody, _ := json.Marshal(scriptSubmission([]byte(validCampaign), runner.ImageID()))
	for _, bad := range []string{
		strings.Replace(string(validBody), `"version":"1"`, `"version":"latest"`, 1),
		strings.Replace(string(validBody), `"script":"campaign_migration"`, `"source":"import os"`, 1),
		strings.Replace(string(validBody), `"version":"1"`, `"version":"1","version":"1"`, 1),
	} {
		if status, _ := post([]byte(bad), "invalid-script"); status != 400 {
			t.Fatal("invalid metadata reached the queue", status)
		}
	}
	assertJobCount(t, pool, 0)
	var fixtures []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
		Status  jobs.State      `json:"status"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(campaignFixtures, &fixtures); err != nil || len(fixtures) != 6 {
		t.Fatal("fixtures", err)
	}
	for index, fixture := range fixtures {
		if !t.Run(fixture.Name, func(t *testing.T) {
			submission := scriptSubmission(fixture.Payload, runner.ImageID())
			body, _ := json.Marshal(submission)
			status, original := post(body, "script-"+fixture.Name)
			if status != 202 || original.Status != jobs.Queued || original.Attempts != 0 {
				t.Fatal("submission", status, original)
			}
			status, replay := post(body, "script-"+fixture.Name)
			if status != 202 || replay.ID != original.ID {
				t.Fatal("duplicate created", status, replay)
			}
			changed, _ := json.Marshal(scriptSubmission(fixture.Payload, scriptTestImage))
			if status, _ := post(changed, "script-"+fixture.Name); status != 409 {
				t.Fatal("changed runtime pin did not conflict", status)
			}
			wantAttempts := 1
			if index == 0 {
				persistedBefore, err := store.Get(ctx, original.ID)
				if err != nil {
					t.Fatal(err)
				}
				// Simulated infrastructure outage before execution; a new real CLI
				// process executes the unchanged pinned script after actual backoff.
				p := worker.VersionedProcessor{NewRunner: func(context.Context) (worker.ScriptRunner, error) {
					return nil, errors.New("synthetic unavailable runner")
				}}
				w, err := worker.New(store, p, worker.ScriptConfig())
				if err != nil {
					t.Fatal(err)
				}
				if worked, err := w.RunOnce(ctx); err != nil || !worked {
					t.Fatal(worked, err)
				}
				waiting, err := store.Get(ctx, original.ID)
				if err != nil || waiting.Status != jobs.RetryWaiting || waiting.FailureCode != jobs.TransientFailure {
					t.Fatal(waiting, err)
				}
				// Compare storage bytes to storage bytes: HTTP compacts RawMessage
				// whitespace, while PostgreSQL renders JSONB with spaces.
				if string(waiting.Payload) != string(persistedBefore.Payload) {
					t.Fatal("retry changed pinned input")
				}
				waitForDatabaseTime(t, ctx, store, original.ID, "available_at")
				wantAttempts = 2
			}
			runWorker()
			runWorker() // Restart on a terminal job must not execute again.
			var completed jobs.Job
			get("/jobs/"+original.ID, &completed)
			if completed.Status != fixture.Status || completed.Attempts != wantAttempts || completed.CompletedAt == nil {
				t.Fatal("completion", completed)
			}
			if fixture.Status == jobs.Succeeded {
				var result struct {
					Output           json.RawMessage `json:"output"`
					Script           string          `json:"script"`
					Version          string          `json:"version"`
					SourceSHA256     string          `json:"source_sha256"`
					ImageID          string          `json:"image_id"`
					CleanupConfirmed bool            `json:"cleanup_confirmed"`
				}
				if json.Unmarshal(completed.Result, &result) != nil || result.Script != scripts.CampaignName || result.Version != scripts.CampaignVersion || result.SourceSHA256 != scripts.CampaignSHA256 || result.ImageID != runner.ImageID() || !result.CleanupConfirmed {
					t.Fatal("missing execution evidence", string(completed.Result))
				}
				var got, want any
				_ = json.Unmarshal(result.Output, &got)
				_ = json.Unmarshal(fixture.Result, &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("output=%s expected=%s", result.Output, fixture.Result)
				}
			} else if completed.FailureCode != jobs.ScriptFailed || len(completed.Result) != 0 {
				t.Fatal("bad permanent script outcome", completed)
			}
			var history struct {
				Attempts []jobs.Attempt `json:"attempts"`
			}
			get("/jobs/"+original.ID+"/attempts", &history)
			if len(history.Attempts) != wantAttempts || history.Attempts[wantAttempts-1].Status != string(fixture.Status) || history.Attempts[wantAttempts-1].FailureCode != completed.FailureCode {
				t.Fatal("history", history)
			}
			if index == 0 && history.Attempts[0].FailureCode != jobs.TransientFailure {
				t.Fatal("lost outage history")
			}
			status, replay = post(body, "script-"+fixture.Name)
			if status != 202 || replay.ID != original.ID || replay.Status != completed.Status || string(replay.Result) != string(completed.Result) || replay.Attempts != wantAttempts {
				t.Fatal("replay lost persisted state", replay)
			}
			t.Logf("script %s: status=%s attempts=%d source=%s runtime=%s", fixture.Name, completed.Status, completed.Attempts, scripts.CampaignSHA256, runner.ImageID())
		}) {
			return
		}
	}
	// An HTTP/store restart must read the same six durable jobs.
	server.Close()
	reopened, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	server = httptest.NewServer(httpapi.NewHandler(reopened, NewJobStore(reopened), logger))
	defer server.Close()
	firstBody, _ := json.Marshal(scriptSubmission(fixtures[0].Payload, runner.ImageID()))
	if status, job := post(firstBody, "script-"+fixtures[0].Name); status != 202 || job.Status != jobs.Succeeded || job.Attempts != 2 {
		t.Fatal("restart replay", status, job)
	}
	assertJobCount(t, pool, 6)
	// A valid but different image ID is never silently replaced by the local tag.
	wrongBody, _ := json.Marshal(scriptSubmission([]byte(validCampaign), scriptTestImage))
	status, wrong := post(wrongBody, "wrong-image-pin")
	if status != 202 {
		t.Fatal(status)
	}
	runWorker()
	var refused jobs.Job
	get("/jobs/"+wrong.ID, &refused)
	if refused.Status != jobs.Failed || refused.FailureCode != jobs.ScriptVersionUnavailable || refused.Attempts != 1 {
		t.Fatal("runtime substitution", refused)
	}
}

func TestScriptMigrationPreservesAllRowsIntegration(t *testing.T) {
	pool, _ := unmigratedIsolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations(name text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_create_jobs.sql", "002_job_idempotency.sql", "003_job_execution.sql", "004_retry_and_attempts.sql"} {
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations(name) VALUES($1)", name); err != nil {
			t.Fatal(err)
		}
	}
	store := NewJobStore(pool)
	for _, out := range []jobs.Outcome{{Status: jobs.Succeeded, Result: []byte(`{}`)}, {Status: jobs.Failed, FailureCode: jobs.InvalidCampaign}, {Status: jobs.Failed, FailureCode: jobs.TransientFailure}} {
		createExecutionJob(t, store, validCampaign)
		claim, err := store.Claim(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, claim, out); err != nil {
			t.Fatal(err)
		}
	}
	createExecutionJob(t, store, validCampaign)
	if _, err := store.Claim(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	createExecutionJob(t, store, validCampaign)
	const snapshot = `SELECT (SELECT jsonb_agg(to_jsonb(j) ORDER BY id)::text FROM jobs j) || (SELECT jsonb_agg(to_jsonb(a) ORDER BY job_id,number)::text FROM job_attempts a)`
	var before, after string
	if err := pool.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, snapshot).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("script migration changed legacy jobs or history")
	}
}
