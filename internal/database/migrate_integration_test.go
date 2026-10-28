package database

import (
	"context"
	"testing"
	"time"

	"durable-jobs/internal/jobs"
)

func TestIdempotencyMigrationPreservesJobsIntegration(t *testing.T) {
	pool, _ := unmigratedIsolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Reconstruct the previously shipped schema using its immutable migration,
	// then add a legacy job before applying the new migration.
	firstSQL, err := migrations.ReadFile("migrations/001_create_jobs.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(firstSQL)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migrations (name) VALUES ('001_create_jobs.sql')`); err != nil {
		t.Fatal(err)
	}
	var legacyID string
	if err := pool.QueryRow(ctx, `INSERT INTO jobs (type, payload)
		VALUES ('migrate_campaign', '{"campaign_id":"synthetic_legacy"}')
		RETURNING id::text`).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	// Read only the columns that existed before the execution migration.
	var before jobs.Job
	if err := pool.QueryRow(ctx, `SELECT id::text, type, payload, status, created_at
		FROM jobs WHERE id = $1`, legacyID).Scan(&before.ID, &before.Type, &before.Payload,
		&before.Status, &before.CreatedAt); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	after, err := NewJobStore(pool).Get(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.Type != before.Type || string(after.Payload) != string(before.Payload) || after.Status != before.Status || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("migration changed legacy job: before=%+v after=%+v", before, after)
	}
	var missingKey bool
	if err := pool.QueryRow(ctx, "SELECT idempotency_key IS NULL FROM jobs WHERE id = $1", legacyID).Scan(&missingKey); err != nil {
		t.Fatal(err)
	}
	if !missingKey {
		t.Fatal("migration invented an idempotency key")
	}
	first, err := NewJobStore(pool).Create(ctx, submission(`{"campaign_id":"synthetic_legacy"}`), "new-key")
	if err != nil || first.ID == legacyID {
		t.Fatalf("new key affected old job: %+v %v", first, err)
	}
	assertJobCount(t, pool, 2)
}

func TestExecutionMigrationPreservesKeyedJobsIntegration(t *testing.T) {
	pool, _ := unmigratedIsolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_create_jobs.sql", "002_job_idempotency.sql"} {
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
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO jobs(type,payload,idempotency_key)
		VALUES('migrate_campaign',$1::jsonb,'legacy-key') RETURNING id::text`, validCampaign).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var before, after string
	query := `SELECT row(id,type,payload,status,created_at,idempotency_key)::text FROM jobs WHERE id=$1`
	if err := pool.QueryRow(ctx, query, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, query, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("execution migration changed previously stored values")
	}
	job, err := NewJobStore(pool).Create(ctx, submission(validCampaign), "legacy-key")
	if err != nil || job.ID != id || job.Attempts != 0 || job.Status != jobs.Queued || job.StartedAt != nil || job.CompletedAt != nil || job.LeaseExpiresAt != nil || len(job.Result) != 0 || job.FailureCode != "" {
		t.Fatalf("legacy job was not preserved as unclaimed: %+v %v", job, err)
	}
}

func TestRetryMigrationBackfillsWithoutReopeningJobsIntegration(t *testing.T) {
	pool, _ := unmigratedIsolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations(name text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_create_jobs.sql", "002_job_idempotency.sql", "003_job_execution.sql"} {
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
	ids := map[string]string{}
	for _, state := range []string{"queued", "running", "succeeded", "failed"} {
		var id string
		err := pool.QueryRow(ctx, `INSERT INTO jobs(type,payload,status,idempotency_key,attempts,started_at,completed_at,lease_token,lease_expires_at,result,failure_code)
			VALUES('migrate_campaign',$2::jsonb,$1,$1,CASE WHEN $1='queued' THEN 0 ELSE 1 END,
			CASE WHEN $1='queued' THEN NULL ELSE clock_timestamp()-interval '1 minute' END,
			CASE WHEN $1 IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END,
			CASE WHEN $1='running' THEN gen_random_uuid() ELSE NULL END,
			CASE WHEN $1='running' THEN clock_timestamp()+interval '1 minute' ELSE NULL END,
			CASE WHEN $1='succeeded' THEN '{}'::jsonb ELSE NULL END,
			CASE WHEN $1='failed' THEN 'processing_timeout' ELSE NULL END)
			RETURNING id::text`, state, validCampaign).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids[state] = id
	}
	const snapshotSQL = `SELECT md5(string_agg(row(id,type,payload,status,created_at,idempotency_key,attempts,started_at,completed_at,lease_token,lease_expires_at,result,failure_code)::text,'|' ORDER BY id)) FROM jobs`
	var before, after string
	if err := pool.QueryRow(ctx, snapshotSQL).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, snapshotSQL).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("retry migration altered existing job fields")
	}
	store := NewJobStore(pool)
	for state, id := range ids {
		job, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if string(job.Status) != state || job.MaxAttempts != 3 || !job.AvailableAt.Equal(job.CreatedAt) {
			t.Fatalf("upgraded job: %+v", job)
		}
		history, err := store.ListAttempts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if state == "queued" {
			if len(history) != 0 {
				t.Fatal("invented queued attempt")
			}
			continue
		}
		if len(history) != 1 || history[0].Number != 1 || history[0].Status != state || !history[0].StartedAt.Equal(*job.StartedAt) {
			t.Fatalf("backfill=%+v", history)
		}
		if state == "running" {
			if history[0].LeaseExpiresAt == nil || history[0].FinishedAt != nil {
				t.Fatal("lost active claim metadata")
			}
		} else if history[0].LeaseExpiresAt != nil || history[0].FinishedAt == nil {
			t.Fatal("invented historical expiry or lost completion")
		}
	}
	// A pre-upgrade running claim can still complete; terminal timeout stays failed.
	var token string
	if err := pool.QueryRow(ctx, "SELECT lease_token::text FROM jobs WHERE id=$1", ids["running"]).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, jobs.Claim{Job: jobs.Job{ID: ids["running"]}, Token: token}, jobs.Outcome{Status: jobs.Succeeded, Result: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpired(ctx, 10); err != nil || n != 0 {
		t.Fatalf("reopened terminal work: %d %v", n, err)
	}
}
