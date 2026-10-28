package database

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/worker"
)

type crashProbe struct{}

func (crashProbe) Process(ctx context.Context, _ jobs.Job) (jobs.Outcome, error) {
	// Worker only enters Process after its claim AND ledger entry commit.
	fmt.Fprintln(os.Stdout, "CLAIM_COMMITTED")
	<-ctx.Done()
	return jobs.Outcome{}, ctx.Err()
}

// Invoked only as an owned child test process. Never install a crash switch in
// the real worker. The schema guard prevents this helper touching application jobs.
func TestCrashWorkerHelper(t *testing.T) {
	if os.Getenv("DURABLE_JOBS_CRASH_HELPER") != "1" {
		t.Skip("owned subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var schema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(schema, "test_jobs_") || schema != os.Getenv("DURABLE_JOBS_TEST_SCHEMA") {
		t.Fatal("refusing non-test schema")
	}
	config := worker.DefaultConfig()
	config.LeaseDuration = 3 * time.Second
	config.ProcessingTimeout = 2 * time.Second
	config.StorageTimeout = 100 * time.Millisecond
	w, err := worker.New(NewJobStore(pool), crashProbe{}, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitForDatabaseTime(t *testing.T, ctx context.Context, store *JobStore, id, column string) {
	t.Helper()
	if column != "lease_expires_at" && column != "available_at" {
		t.Fatal("unexpected test clock column")
	}
	for {
		var due bool
		if err := store.pool.QueryRow(ctx, "SELECT "+column+" <= clock_timestamp() FROM jobs WHERE id=$1", id).Scan(&due); err != nil {
			t.Fatal(err)
		}
		if due {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestKilledWorkerRecoveredByNewProcessIntegration(t *testing.T) {
	pool, config := isolatedDatabase(t)
	store := NewJobStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	created := createExecutionJob(t, store, validCampaign)
	baseEnv := []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "DATABASE_URL=") && !strings.HasPrefix(value, "PGOPTIONS=") && !strings.HasPrefix(value, "DURABLE_JOBS_CRASH_HELPER=") && !strings.HasPrefix(value, "DURABLE_JOBS_TEST_SCHEMA=") {
			baseEnv = append(baseEnv, value)
		}
	}
	schema := config.ConnConfig.RuntimeParams["search_path"]
	baseEnv = append(baseEnv, "DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"), "PGOPTIONS=-c search_path="+schema)
	binary := filepath.Join(t.TempDir(), "worker")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/worker")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	helper := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashWorkerHelper$")
	helper.Env = append(append([]string{}, baseEnv...), "DURABLE_JOBS_CRASH_HELPER=1", "DURABLE_JOBS_TEST_SCHEMA="+schema)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if helper.ProcessState == nil {
			_ = helper.Process.Kill()
			_ = helper.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "CLAIM_COMMITTED" {
		t.Fatalf("helper did not confirm claim: %q %v", scanner.Text(), scanner.Err())
	}
	// Kill exactly the child started above, not a discovered/user-owned process.
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatal("helper was not killed")
	}
	before, err := store.Get(ctx, created.ID)
	if err != nil || before.Status != jobs.Running || before.Attempts != 1 {
		t.Fatalf("after kill: %+v %v", before, err)
	}
	waitForDatabaseTime(t, ctx, store, created.ID, "lease_expires_at")
	run := func() {
		command := exec.CommandContext(ctx, binary, "-once")
		command.Env = baseEnv
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("restarted worker: %v %s", err, output)
		}
	}
	run() // Recovery closes attempt 1 and schedules, but does not skip backoff.
	waiting, err := store.Get(ctx, created.ID)
	if err != nil || waiting.Status != jobs.RetryWaiting || waiting.Attempts != 1 {
		t.Fatalf("recovered: %+v %v", waiting, err)
	}
	waitForDatabaseTime(t, ctx, store, created.ID, "available_at")
	run() // New OS process executes attempt 2 with a new lease.
	after, err := store.Get(ctx, created.ID)
	if err != nil || after.Status != jobs.Succeeded || after.Attempts != 2 || len(after.Result) == 0 {
		t.Fatalf("final: %+v %v", after, err)
	}
	history, err := store.ListAttempts(ctx, created.ID)
	if err != nil || len(history) != 2 || history[0].Status != jobs.LeaseExpired || history[1].Status != "succeeded" {
		t.Fatalf("history: %+v %v", history, err)
	}
	t.Log("killed an owned worker process after committed claim; two fresh CLI worker processes recovered and completed the same job after real expiry/backoff")
}
