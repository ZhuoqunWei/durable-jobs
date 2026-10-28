package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"durable-jobs/internal/httpapi"
	"durable-jobs/internal/jobs"
	"durable-jobs/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Test-only clock advancement inside the owned schema. The application never
// rewrites leases or retry deadlines this way. Keep job/history expiry aligned.
func expireClaim(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `UPDATE jobs SET lease_expires_at=(SELECT started_at+interval '1 microsecond'
		FROM job_attempts WHERE job_id=jobs.id AND number=jobs.attempts) WHERE id=$1 AND status='running'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE job_attempts SET lease_expires_at=(SELECT lease_expires_at FROM jobs WHERE id=$1)
		WHERE job_id=$1 AND status='running'`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func makeRetryDue(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE id=$1 AND status='retry_waiting'`, id); err != nil {
		t.Fatal(err)
	}
}

func TestRetryBudgetAndHistoryIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := store.Create(ctx, submission(validCampaign), "retry-budget")
	if err != nil {
		t.Fatal(err)
	}
	if created.MaxAttempts != jobs.DefaultMaxAttempts {
		t.Fatalf("default attempts=%d", created.MaxAttempts)
	}
	empty, err := store.ListAttempts(ctx, created.ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("queued history: %+v %v", empty, err)
	}
	var previousToken string
	var firstStart time.Time
	for attempt := 1; attempt <= 3; attempt++ {
		claim, err := store.Claim(ctx, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if claim.Job.Attempts != attempt || claim.Token == previousToken || claim.Job.FailureCode != "" {
			t.Fatalf("claim=%+v", claim.Job)
		}
		previousToken = claim.Token
		if attempt == 1 {
			firstStart = *claim.Job.StartedAt
		} else if !claim.Job.StartedAt.Equal(firstStart) {
			t.Fatal("job lost its first start timestamp")
		}
		if err := store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.TransientFailure}); err != nil {
			t.Fatal(err)
		}
		got, err := store.Create(ctx, submission(validCampaign), "retry-budget")
		if err != nil {
			t.Fatal(err)
		}
		want := jobs.RetryWaiting
		if attempt == 3 {
			want = jobs.Failed
		}
		if got.ID != created.ID || got.Status != want || got.Attempts != attempt || got.FailureCode != jobs.TransientFailure || got.LeaseExpiresAt != nil || len(got.Result) != 0 {
			t.Fatalf("after attempt %d: %+v", attempt, got)
		}
		history, err := store.ListAttempts(ctx, created.ID)
		if err != nil || len(history) != attempt {
			t.Fatalf("history=%+v err=%v", history, err)
		}
		last := history[len(history)-1]
		if last.Number != attempt || last.Status != "failed" || last.FinishedAt == nil || last.FailureCode != jobs.TransientFailure {
			t.Fatalf("attempt=%+v", last)
		}
		if _, err := store.Claim(ctx, 30*time.Second); !errors.Is(err, jobs.ErrNoQueuedJob) {
			t.Fatalf("early/fourth claim: %v", err)
		}
		if attempt < 3 {
			if got.CompletedAt != nil || !got.AvailableAt.After(*last.FinishedAt) {
				t.Fatal("retry has no backoff or is terminal")
			}
			makeRetryDue(t, pool, created.ID)
		} else if got.CompletedAt == nil {
			t.Fatal("exhausted job is not terminal")
		}
	}
	assertJobCount(t, pool, 1)
}

func TestPermanentFailuresDoNotRetryIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, code := range []string{jobs.InvalidCampaign, jobs.UnsupportedJobType, jobs.ProcessingFailed} {
		createExecutionJob(t, store, validCampaign)
		claim, err := store.Claim(ctx, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Failed, FailureCode: code}); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, claim.Job.ID)
		if err != nil || got.Status != jobs.Failed || got.Attempts != 1 || got.CompletedAt == nil {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if _, err := store.Claim(ctx, 30*time.Second); !errors.Is(err, jobs.ErrNoQueuedJob) {
		t.Fatal(err)
	}
}

func TestExpiredClaimRecoveryFencesOldWorkerIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created := createExecutionJob(t, store, validCampaign)
	old, err := store.Claim(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	expireClaim(t, pool, created.ID)
	// Reopen independent connections, as after loss of the original worker.
	reopened, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewJobStore(reopened)
	if n, err := restarted.RecoverExpired(ctx, 10); err != nil || n != 1 {
		t.Fatalf("recovery=%d %v", n, err)
	}
	if n, err := restarted.RecoverExpired(ctx, 10); err != nil || n != 0 {
		t.Fatalf("duplicate recovery=%d %v", n, err)
	}
	success := jobs.Outcome{Status: jobs.Succeeded, Result: json.RawMessage(`{"recovered":true}`)}
	if err := store.Finish(ctx, old, success); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("old worker completed waiting job: %v", err)
	}
	makeRetryDue(t, pool, created.ID)
	current, err := restarted.Claim(ctx, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if current.Token == old.Token || current.Job.Attempts != 2 {
		t.Fatal("retry reused claim authority")
	}
	if err := store.Finish(ctx, old, success); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("old worker completed new attempt: %v", err)
	}
	if err := restarted.Finish(ctx, current, success); err != nil {
		t.Fatal(err)
	}
	history, err := restarted.ListAttempts(ctx, created.ID)
	if err != nil || len(history) != 2 || history[0].Status != jobs.LeaseExpired || history[1].Status != "succeeded" {
		t.Fatalf("history=%+v %v", history, err)
	}
	handler := httpapi.NewHandler(reopened, restarted, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/jobs/"+created.ID+"/attempts", nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), old.Token) || strings.Contains(response.Body.String(), current.Token) || strings.Contains(response.Body.String(), "lease_token") {
		t.Fatalf("unsafe history: %s", response.Body.String())
	}
}

func TestRepeatedCrashesExhaustBudgetIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created := createExecutionJob(t, store, validCampaign)
	for i := 1; i <= 3; i++ {
		if _, err := store.Claim(ctx, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		expireClaim(t, pool, created.ID)
		if n, err := store.RecoverExpired(ctx, 10); err != nil || n != 1 {
			t.Fatalf("recover %d: %d %v", i, n, err)
		}
		if i < 3 {
			makeRetryDue(t, pool, created.ID)
		}
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil || got.Status != jobs.Failed || got.Attempts != 3 || got.FailureCode != jobs.LeaseExpired || got.CompletedAt == nil {
		t.Fatalf("exhausted: %+v %v", got, err)
	}
	if _, err := store.Claim(ctx, 30*time.Second); !errors.Is(err, jobs.ErrNoQueuedJob) {
		t.Fatal("claimed a fourth attempt")
	}
	history, err := store.ListAttempts(ctx, created.ID)
	if err != nil || len(history) != 3 {
		t.Fatalf("%+v %v", history, err)
	}
	for _, attempt := range history {
		if attempt.Status != jobs.LeaseExpired || attempt.FinishedAt == nil {
			t.Fatalf("unfinished expired attempt: %+v", attempt)
		}
	}
}

func TestRecoveryBatchConcurrencyAndLocksIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ids []string
	for i := 0; i < 5; i++ {
		created := createExecutionJob(t, store, validCampaign)
		ids = append(ids, created.ID)
		if _, err := store.Claim(ctx, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if i < 4 {
			expireClaim(t, pool, created.ID)
		}
	}
	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(owner)
	if _, err := owner.Exec(ctx, "SELECT id FROM jobs WHERE id=$1 FOR UPDATE", ids[0]); err != nil {
		t.Fatal(err)
	}
	secondPool, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer secondPool.Close()
	stores := []*JobStore{store, NewJobStore(secondPool)}
	type result struct {
		n   int
		err error
	}
	results := make(chan result, 16)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			n, err := stores[i%2].RecoverExpired(ctx, 1)
			results <- result{n, err}
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	total := 0
	for r := range results {
		if r.err != nil || r.n > 1 {
			t.Fatalf("batch result: %+v", r)
		}
		total += r.n
	}
	if total != 3 {
		t.Fatalf("recovered=%d want 3 unlocked expired jobs", total)
	}
	if err := owner.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpired(ctx, 1); err != nil || n != 1 {
		t.Fatalf("released recovery: %d %v", n, err)
	}
	if n, err := store.RecoverExpired(ctx, 100); err != nil || n != 0 {
		t.Fatalf("live lease recovered: %d %v", n, err)
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err := store.RecoverExpired(ctx, limit); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
}

func TestAttemptHistoryCommitFailuresRollBackTogetherIntegration(t *testing.T) {
	for _, stage := range []string{"claim", "finish", "recover"} {
		t.Run(stage, func(t *testing.T) {
			pool, _ := isolatedDatabase(t)
			store := NewJobStore(pool)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			created := createExecutionJob(t, store, validCampaign)
			var claim jobs.Claim
			var err error
			if stage != "claim" {
				claim, err = store.Claim(ctx, 30*time.Second)
				if err != nil {
					t.Fatal(err)
				}
			}
			if stage == "recover" {
				expireClaim(t, pool, created.ID)
			}
			event := "UPDATE"
			if stage == "claim" {
				event = "INSERT"
			}
			if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_history_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'synthetic ledger commit failure'; END $$;
			CREATE CONSTRAINT TRIGGER reject_history_commit AFTER `+event+` ON job_attempts
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_history_commit()`); err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "claim":
				claim, err = store.Claim(ctx, 30*time.Second)
				if err == nil || claim.Token != "" {
					t.Fatal("uncommitted claim escaped")
				}
			case "finish":
				if err := store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Failed, FailureCode: jobs.TransientFailure}); err == nil {
					t.Fatal("failed history commit acknowledged")
				}
			case "recover":
				if n, err := store.RecoverExpired(ctx, 10); err == nil || n != 0 {
					t.Fatal("uncommitted recovery counted")
				}
			}
			got, err := store.Get(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			history, err := store.ListAttempts(ctx, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "claim" {
				if got.Status != jobs.Queued || got.Attempts != 0 || len(history) != 0 {
					t.Fatalf("partial claim: %+v %+v", got, history)
				}
			} else {
				if got.Status != jobs.Running || len(history) != 1 || history[0].Status != "running" || history[0].FinishedAt != nil {
					t.Fatalf("partial completion: %+v %+v", got, history)
				}
			}
		})
	}
}

type flakyProcessor struct{ calls int }

func (p *flakyProcessor) Process(ctx context.Context, job jobs.Job) (jobs.Outcome, error) {
	p.calls++
	if p.calls == 1 {
		return jobs.Outcome{}, jobs.ErrTransient
	}
	return (worker.CampaignProcessor{}).Process(ctx, job)
}

func TestWorkerRetryThenSuccessIntegration(t *testing.T) {
	pool, _ := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created := createExecutionJob(t, store, validCampaign)
	p := &flakyProcessor{}
	w, err := worker.New(store, p, worker.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := w.RunOnce(ctx); !worked || err != nil {
		t.Fatalf("first attempt: %v %v", worked, err)
	}
	if worked, err := w.RunOnce(ctx); worked || err != nil {
		t.Fatalf("backoff ignored: %v %v", worked, err)
	}
	makeRetryDue(t, pool, created.ID)
	if worked, err := w.RunOnce(ctx); !worked || err != nil {
		t.Fatalf("second attempt: %v %v", worked, err)
	}
	got, err := store.Get(ctx, created.ID)
	if err != nil || got.Status != jobs.Succeeded || got.Attempts != 2 || len(got.Result) == 0 || p.calls != 2 {
		t.Fatalf("result=%+v calls=%d %v", got, p.calls, err)
	}
	history, err := store.ListAttempts(ctx, created.ID)
	if err != nil || len(history) != 2 || history[0].FailureCode != jobs.TransientFailure || history[1].Status != "succeeded" {
		t.Fatalf("%+v %v", history, err)
	}
}

func TestRecoveryRacesCompletionIntegration(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			pool, config := isolatedDatabase(t)
			store := NewJobStore(pool)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			created := createExecutionJob(t, store, validCampaign)
			claim, err := store.Claim(ctx, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if expired {
				expireClaim(t, pool, created.ID)
			}
			other, err := pgxpool.NewWithConfig(ctx, config.Copy())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			start := make(chan struct{})
			finished := make(chan error, 1)
			type recoveryResult struct {
				n   int
				err error
			}
			recovered := make(chan recoveryResult, 1)
			go func() {
				<-start
				finished <- store.Finish(ctx, claim, jobs.Outcome{Status: jobs.Succeeded, Result: json.RawMessage(`{}`)})
			}()
			go func() {
				<-start
				n, err := NewJobStore(other).RecoverExpired(ctx, 10)
				recovered <- recoveryResult{n, err}
			}()
			close(start)
			finishErr := <-finished
			r := <-recovered
			if r.err != nil {
				t.Fatal(r.err)
			}
			if expired {
				if !errors.Is(finishErr, jobs.ErrLeaseLost) {
					t.Fatalf("expired completion: %v", finishErr)
				}
				// Recovery may skip a row momentarily locked by rejected completion.
				n, err := store.RecoverExpired(ctx, 10)
				if err != nil || r.n+n != 1 {
					t.Fatalf("recover count=%d %d %v", r.n, n, err)
				}
			} else if finishErr != nil || r.n != 0 {
				t.Fatalf("live completion: %v recovery=%d", finishErr, r.n)
			}
			history, err := store.ListAttempts(ctx, created.ID)
			if err != nil || len(history) != 1 {
				t.Fatalf("%+v %v", history, err)
			}
			want := "succeeded"
			if expired {
				want = jobs.LeaseExpired
			}
			if history[0].Status != want {
				t.Fatalf("history=%+v", history)
			}
		})
	}
}
