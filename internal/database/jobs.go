package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"durable-jobs/internal/jobs"
	"durable-jobs/internal/scripts"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type JobStore struct{ pool *pgxpool.Pool }

const jobColumns = `id::text, type, payload, status, created_at, attempts,
	started_at, completed_at, lease_expires_at, result, COALESCE(failure_code, ''), max_attempts, available_at`

func jobDestinations(job *jobs.Job) []any {
	return []any{&job.ID, &job.Type, &job.Payload, &job.Status, &job.CreatedAt,
		&job.Attempts, &job.StartedAt, &job.CompletedAt, &job.LeaseExpiresAt, &job.Result, &job.FailureCode,
		&job.MaxAttempts, &job.AvailableAt}
}

func NewJobStore(pool *pgxpool.Pool) *JobStore { return &JobStore{pool: pool} }

// Create inserts a job or returns the original for the same key and input.
// An empty key opts out. Non-empty keys remain reserved for the row's lifetime.
func (s *JobStore) Create(ctx context.Context, request jobs.SubmitRequest, key string) (jobs.Job, error) {
	if key != "" {
		if err := jobs.ValidateIdempotencyKey(key); err != nil {
			return jobs.Job{}, err
		}
	}
	// The SELECT after a conflicting INSERT needs a fresh statement snapshot,
	// including when a concurrent transaction committed while INSERT waited.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return jobs.Job{}, fmt.Errorf("begin job creation: %w", err)
	}
	defer rollback(tx)
	// Keep durable local commits even if a session default changes. Hardware and
	// database fsync guarantees still matter; this is not a backup or replication.
	if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); err != nil {
		return jobs.Job{}, fmt.Errorf("set commit durability: %w", err)
	}
	var job jobs.Job
	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (type, payload, idempotency_key)
		VALUES ($1, $2::jsonb, NULLIF($3, ''))
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING `+jobColumns,
		request.Type, string(request.Payload), key).Scan(jobDestinations(&job)...)
	if errors.Is(err, pgx.ErrNoRows) {
		// This MUST be a separate statement, not a SELECT in the INSERT's CTE:
		// ON CONFLICT can see a competing row that its old snapshot cannot read.
		var sameInput bool
		err = tx.QueryRow(ctx, `
			SELECT `+jobColumns+`,
			       type = $2 AND payload = $3::jsonb
			FROM jobs WHERE idempotency_key = $1`, key, request.Type, string(request.Payload)).
			Scan(append(jobDestinations(&job), &sameInput)...)
		if err != nil {
			return jobs.Job{}, creationError("read existing submission", err)
		}
		if !sameInput {
			return jobs.Job{}, jobs.ErrIdempotencyConflict
		}
	}
	if err != nil {
		return jobs.Job{}, creationError("insert job", err)
	}
	// JSONB may expand numeric exponents/whitespace. Verify the representation
	// the worker will actually read before accepting or committing a script job.
	if job.Type == "run_script" {
		if _, err := scripts.Parse(job.Payload); err != nil {
			return jobs.Job{}, jobs.ErrInvalidPayload
		}
	}
	// The HTTP layer only receives a successful result AFTER COMMIT returns.
	if err := tx.Commit(ctx); err != nil {
		return jobs.Job{}, fmt.Errorf("commit job: %w", err)
	}
	job.CreatedAt = job.CreatedAt.UTC()
	return job, nil
}

func creationError(operation string, err error) error {
	var pgError *pgconn.PgError
	if errors.As(err, &pgError) {
		switch pgError.Code {
		case "22P02", "22P05", "22021", "22003":
			return jobs.ErrInvalidPayload
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (s *JobStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	var job jobs.Job
	err := s.pool.QueryRow(ctx, `
		SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id).
		Scan(jobDestinations(&job)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, fmt.Errorf("get job: %w", err)
	}
	job.CreatedAt = job.CreatedAt.UTC()
	return job, nil
}

func rollback(tx pgx.Tx) {
	// Request cancellation must not prevent cleanup of its transaction.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx) // Harmless when the transaction already committed.
}
