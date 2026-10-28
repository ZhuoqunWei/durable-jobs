package database

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"durable-jobs/internal/httpapi"
	"durable-jobs/internal/jobs"
	"durable-jobs/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

const validCampaign = `{"campaign_id":"synthetic_worker","category":"Pets","created_at":"08/31/2026"}`

func createExecutionJob(t *testing.T, store *JobStore, payload string) jobs.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job, err := store.Create(ctx, submission(payload), "")
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestConcurrentJobClaimsIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	secondPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer secondPool.Close()
	stores := []*JobStore{NewJobStore(pool), NewJobStore(secondPool)}
	created := createExecutionJob(t, stores[0], validCampaign)
	type result struct {
		claim jobs.Claim
		err   error
	}
	results := make(chan result, 16)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			c, e := stores[i%2].Claim(ctx, 30*time.Second)
			results <- result{c, e}
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	claimed := 0
	for r := range results {
		if errors.Is(r.err, jobs.ErrNoQueuedJob) {
			continue
		}
		if r.err != nil {
			t.Fatal(r.err)
		}
		claimed++
		if r.claim.Job.ID != created.ID || r.claim.Job.Status != jobs.Running || r.claim.Token == "" || r.claim.Job.Attempts != 1 || r.claim.Job.StartedAt == nil || r.claim.Job.LeaseExpiresAt == nil {
			t.Fatalf("bad claim: %+v", r.claim)
		}
	}
	if claimed != 1 {
		t.Fatalf("claims=%d, want 1", claimed)
	}
	assertJobCount(t, pool, 1)
}

func TestClaimSkipsLockedQueuedJobsIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	first := createExecutionJob(t, store, validCampaign)
	second := createExecutionJob(t, store, validCampaign)
	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(owner)
	if _, err := owner.Exec(ctx, "SELECT id FROM jobs WHERE id=$1 FOR UPDATE", first.ID); err != nil {
		t.Fatal(err)
	}
	claimCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	claim, err := store.Claim(claimCtx, 30*time.Second)
	if err != nil || claim.Job.ID != second.ID {
		t.Fatalf("locked oldest job blocked next job: %+v %v", claim, err)
	}
	if _, err := store.Claim(claimCtx, 30*time.Second); !errors.Is(err, jobs.ErrNoQueuedJob) {
		t.Fatalf("all available rows locked: %v", err)
	}
	if err := owner.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	claim, err = store.Claim(ctx, 30*time.Second)
	if err != nil || claim.Job.ID != first.ID {
		t.Fatalf("released row not claimed: %+v %v", claim, err)
	}
}

func TestJobCompletionFencingIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	createExecutionJob(t, store, validCampaign)
	claim, err := store.Claim(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	outcome := jobs.Outcome{Status: jobs.Succeeded, Result: json.RawMessage(`{"synthetic":true}`)}
	wrong := claim
	wrong.Token = "not-the-current-token"
	if err := store.Finish(ctx, wrong, outcome); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("wrong token: %v", err)
	}
	missing := claim
	missing.Job.ID = "00000000-0000-0000-0000-000000000000"
	if err := store.Finish(ctx, missing, outcome); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("missing claim: %v", err)
	}
	if err := store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Succeeded}); !errors.Is(err, jobs.ErrInvalidOutcome) {
		t.Fatalf("invalid result: %v", err)
	}
	// Test-only token rotation; real expired-claim recovery has separate tests.
	newClaim := claim
	if err := pool.QueryRow(ctx, `UPDATE jobs SET lease_token=gen_random_uuid() WHERE id=$1 RETURNING lease_token::text`, claim.Job.ID).Scan(&newClaim.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE job_attempts SET lease_token=$2 WHERE job_id=$1 AND status='running'`, claim.Job.ID, newClaim.Token); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, claim, outcome); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale owner completed: %v", err)
	}
	if err := store.Finish(ctx, newClaim, outcome); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, newClaim, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ProcessingFailed}); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("terminal result overwritten: %v", err)
	}
	got, err := store.Get(ctx, claim.Job.ID)
	if err != nil || got.Status != jobs.Succeeded || got.CompletedAt == nil || got.LeaseExpiresAt != nil || got.Attempts != 1 || len(got.Result) == 0 || got.FailureCode != "" {
		t.Fatalf("completed job: %+v %v", got, err)
	}
}

func TestConcurrentCompletionsAndCanceledWaitIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	createExecutionJob(t, store, validCampaign)
	claim, err := store.Claim(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	outcome := jobs.Outcome{Status: jobs.Succeeded, Result: json.RawMessage(`{}`)}
	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(owner)
	if _, err := owner.Exec(ctx, "SELECT id FROM jobs WHERE id=$1 FOR UPDATE", claim.Job.ID); err != nil {
		t.Fatal(err)
	}
	waitConfig := config.Copy()
	waitConfig.MaxConns = 1
	waiter, err := pgxpool.NewWithConfig(ctx, waitConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	var pid int
	if err := waiter.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- NewJobStore(waiter).Finish(waitCtx, claim, outcome) }()
	waitForLock(t, ctx, pool, pid)
	stop()
	if err := <-done; err == nil {
		t.Fatal("canceled completion succeeded")
	}
	if err := owner.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, claim.Job.ID)
	if err != nil || got.Status != jobs.Running || len(got.Result) != 0 {
		t.Fatalf("canceled wait changed job: %+v %v", got, err)
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	go func() { <-start; results <- store.Finish(ctx, claim, outcome) }()
	go func() {
		<-start
		results <- NewJobStore(waiter).Finish(ctx, claim, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.ProcessingFailed})
	}()
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, jobs.ErrLeaseLost) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("completion winners=%d", winners)
	}
}

func TestFinishRechecksExpiryAfterLockWaitIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	created := createExecutionJob(t, store, validCampaign)
	claim, err := store.Claim(ctx, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(owner)
	if _, err := owner.Exec(ctx, "SELECT id FROM jobs WHERE id=$1 FOR UPDATE", created.ID); err != nil {
		t.Fatal(err)
	}
	waitConfig := config.Copy()
	waitConfig.MaxConns = 1
	waiter, err := pgxpool.NewWithConfig(ctx, waitConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	var pid int
	if err := waiter.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- NewJobStore(waiter).Finish(ctx, claim, jobs.Outcome{Status: jobs.Succeeded, Result: json.RawMessage(`{}`)})
	}()
	waitForLock(t, ctx, pool, pid)
	// Observe database time, not a fixed sleep or the worker machine's clock.
	for {
		var expired bool
		if err := pool.QueryRow(ctx, "SELECT lease_expires_at <= clock_timestamp() FROM jobs WHERE id=$1", created.ID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	// No UPDATE was made by the lock holder: expiry must be checked after waiting.
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("expired completion accepted: %v", err)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil || got.Status != jobs.Running || len(got.Result) != 0 {
		t.Fatalf("expired job: %+v %v", got, err)
	}
	if _, err := store.Claim(ctx, 30*time.Second); !errors.Is(err, jobs.ErrNoQueuedJob) {
		t.Fatalf("expired job was automatically retried: %v", err)
	}
}

func TestExecutionCommitFailuresIntegration(t *testing.T) {
	for _, state := range []string{"running", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			pool, _ := isolatedDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store := NewJobStore(pool)
			created := createExecutionJob(t, store, validCampaign)
			var claim jobs.Claim
			var err error
			if state == "succeeded" {
				claim, err = store.Claim(ctx, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Test-owned schema only: UPDATE succeeds, its deferred COMMIT fails.
			_, err = pool.Exec(ctx, `CREATE FUNCTION reject_execution_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic deferred execution failure'; END $$;
			CREATE CONSTRAINT TRIGGER reject_execution_commit AFTER UPDATE ON jobs
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = '`+state+`')
			EXECUTE FUNCTION reject_execution_commit()`)
			if err != nil {
				t.Fatal(err)
			}
			if state == "running" {
				claim, err = store.Claim(ctx, 30*time.Second)
				if err == nil || claim.Token != "" || claim.Job.ID != "" {
					t.Fatalf("uncommitted claim escaped: %+v %v", claim, err)
				}
			} else {
				err = store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Succeeded, Result: json.RawMessage(`{}`)})
				if err == nil {
					t.Fatal("uncommitted result acknowledged")
				}
			}
			got, err := store.Get(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := jobs.Queued
			if state == "succeeded" {
				want = jobs.Running
			}
			if got.Status != want || got.CompletedAt != nil || len(got.Result) != 0 {
				t.Fatalf("partial execution commit: %+v", got)
			}
		})
	}
}

func TestWorkerProcessesValidAndInvalidJobsIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	valid := createExecutionJob(t, store, validCampaign)
	invalid := createExecutionJob(t, store, `{"campaign_id":"synthetic_bad","category":"Pets","created_at":"02/29/2026"}`)
	w, err := worker.New(store, worker.CampaignProcessor{}, worker.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if worked, err := w.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("worked=%v error=%v", worked, err)
		}
	}
	if worked, err := w.RunOnce(ctx); err != nil || worked {
		t.Fatalf("empty queue: %v %v", worked, err)
	}
	good, err := store.Get(ctx, valid.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ID       string `json:"campaign_id"`
		Category string `json:"category_id"`
		Date     string `json:"created_date"`
		Version  int    `json:"schema_version"`
	}
	if err := json.Unmarshal(good.Result, &result); err != nil {
		t.Fatal(err)
	}
	if good.Status != jobs.Succeeded || result.ID != "synthetic_worker" || result.Category != "animals" || result.Date != "2026-08-31" || result.Version != 1 {
		t.Fatalf("migration output: %+v %+v", good, result)
	}
	bad, err := store.Get(ctx, invalid.ID)
	if err != nil || bad.Status != jobs.Failed || bad.FailureCode != jobs.InvalidCampaign || len(bad.Result) != 0 {
		t.Fatalf("invalid input: %+v %v", bad, err)
	}
	if string(good.Payload) != string(valid.Payload) {
		t.Fatal("migration mutated the original payload")
	}
}

func TestExecutionSchemaConstraintsIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	created := createExecutionJob(t, store, validCampaign)
	for _, change := range []string{"status='succeeded'", "status='running'", "attempts=1", "result='{}'", "failure_code='invalid_campaign'", "lease_token=gen_random_uuid()"} {
		if _, err := pool.Exec(ctx, "UPDATE jobs SET "+change+" WHERE id=$1", created.ID); err == nil {
			t.Fatalf("schema accepted %s", change)
		}
	}
	if _, err := store.Claim(ctx, 0); !errors.Is(err, jobs.ErrInvalidLease) {
		t.Fatalf("invalid lease: %v", err)
	}
	claim, err := store.Claim(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='failed',completed_at=clock_timestamp(),lease_token=NULL,lease_expires_at=NULL WHERE id=$1`, claim.Job.ID); err == nil {
		t.Fatal("failed state accepted without failure code")
	}
}

func TestWorkerBinaryAndHTTPReplayIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store := NewJobStore(pool)
	handler := httpapi.NewHandler(pool, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"type":"migrate_campaign","payload":` + validCampaign + `}`
	response := postJob(handler, body, "synthetic-worker-binary")
	if response.Code != 202 {
		t.Fatalf("submit: %d %s", response.Code, response.Body.String())
	}
	var original jobs.Job
	if err := json.Unmarshal(response.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "worker")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/worker")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	environment := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "DATABASE_URL=") && !strings.HasPrefix(value, "PGOPTIONS=") {
			environment = append(environment, value)
		}
	}
	environment = append(environment, "DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"), "PGOPTIONS=-c search_path="+config.ConnConfig.RuntimeParams["search_path"])
	for i := 0; i < 2; i++ {
		command := exec.CommandContext(ctx, binary, "-once")
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("worker process %d: %v\n%s", i, err, output)
		}
	}
	// A new store/HTTP handler reads output persisted by a separate OS process.
	reopened, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := httpapi.NewHandler(reopened, NewJobStore(reopened), slog.New(slog.NewTextHandler(io.Discard, nil)))
	get := httptest.NewRecorder()
	restarted.ServeHTTP(get, httptest.NewRequest("GET", "/jobs/"+original.ID, nil))
	var completed jobs.Job
	if err := json.Unmarshal(get.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if get.Code != 200 || completed.Status != jobs.Succeeded || completed.Attempts != 1 || len(completed.Result) == 0 {
		t.Fatalf("worker did not persist output: %d %s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), "lease_token") {
		t.Fatal("HTTP exposed worker authority")
	}
	replay := postJob(restarted, body, "synthetic-worker-binary")
	var replayed jobs.Job
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replay.Code != 202 || replayed.ID != original.ID || replayed.Status != jobs.Succeeded || string(replayed.Result) != string(completed.Result) || replayed.Attempts != 1 {
		t.Fatalf("replay lost completed state: %s", replay.Body.String())
	}
	assertJobCount(t, pool, 1)
	t.Log("separate worker process persisted the migration; restarted worker found no queued work; HTTP replay returned the same completed job")
}
