package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"durable-jobs/internal/jobs"
	"github.com/jackc/pgx/v5"
)

// Claim assigns one due queued/retry job. It commits both claim and attempt
// history BEFORE execution. Expired running jobs are handled by RecoverExpired.
func (s *JobStore) Claim(ctx context.Context, leaseDuration time.Duration) (jobs.Claim, error) {
	if err := jobs.ValidateLeaseDuration(leaseDuration); err != nil {
		return jobs.Claim{}, err
	}
	tx, err := s.executionTransaction(ctx)
	if err != nil {
		return jobs.Claim{}, err
	}
	defer rollback(tx)
	var claim jobs.Claim
	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM jobs
		WHERE status IN ('queued','retry_waiting') AND available_at <= clock_timestamp()
			AND attempts < max_attempts
		ORDER BY available_at, created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Claim{}, jobs.ErrNoQueuedJob
	}
	if err != nil {
		return jobs.Claim{}, fmt.Errorf("claim job: %w", err)
	}
	// Use one database timestamp for this attempt, independent of worker clocks.
	var startedAt time.Time
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&startedAt); err != nil {
		return jobs.Claim{}, err
	}
	err = tx.QueryRow(ctx, `UPDATE jobs SET status='running', attempts=attempts+1,
		started_at=COALESCE(started_at,$2), lease_token=gen_random_uuid(),
		lease_expires_at=$2::timestamptz + $3::bigint * interval '1 microsecond',
		failure_code=NULL
		WHERE id=$1 RETURNING `+jobColumns+`, lease_token::text`, id, startedAt, leaseDuration.Microseconds()).
		Scan(append(jobDestinations(&claim.Job), &claim.Token)...)
	if err != nil {
		return jobs.Claim{}, fmt.Errorf("assign claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_attempts(job_id,number,status,started_at,lease_expires_at,lease_token)
		VALUES($1,$2,'running',$3,$4,$5)`, claim.Job.ID, claim.Job.Attempts, startedAt, claim.Job.LeaseExpiresAt, claim.Token); err != nil {
		return jobs.Claim{}, fmt.Errorf("record claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		// Commit may have succeeded. Never execute using an unconfirmed claim.
		return jobs.Claim{}, fmt.Errorf("commit job claim (outcome may be unknown): %w", err)
	}
	return claim, nil
}

// Finish commits the output, terminal/retry state and attempt history together.
// This fences database completion only, not hypothetical external side effects.
func (s *JobStore) Finish(ctx context.Context, claim jobs.Claim, outcome jobs.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	tx, err := s.executionTransaction(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	// Acquire the lock first, then check wall-clock expiry in a fresh statement.
	// A check made before waiting on a lock could accept an already expired lease.
	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, "SELECT attempts,max_attempts FROM jobs WHERE id = $1 FOR UPDATE", claim.Job.ID).Scan(&attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock job completion: %w", err)
	}
	var result any
	if outcome.Status == jobs.Succeeded {
		result = string(outcome.Result)
	}
	next, delay := outcome.Status, time.Duration(0)
	if outcome.Status == jobs.Failed {
		next, delay = jobs.AfterFailure(outcome.FailureCode, attempts, maxAttempts)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE jobs SET status = $3, result = $4::jsonb, failure_code = NULLIF($5, ''),
			completed_at = CASE WHEN $3='retry_waiting' THEN NULL ELSE clock_timestamp() END,
			available_at = CASE WHEN $3='retry_waiting' THEN clock_timestamp()+$6::bigint*interval '1 microsecond' ELSE available_at END,
			lease_token = NULL, lease_expires_at = NULL
		WHERE id = $1 AND status = 'running' AND lease_token::text = $2
			AND lease_expires_at > clock_timestamp()`,
		claim.Job.ID, claim.Token, next, result, outcome.FailureCode, delay.Microseconds())
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return jobs.ErrLeaseLost
	}
	if err := recordAttemptEnd(ctx, tx, claim.Job.ID, attempts, claim.Token, string(outcome.Status), outcome.FailureCode); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit job completion (outcome may be unknown): %w", err)
	}
	return nil
}

func (s *JobStore) executionTransaction(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin execution transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); err != nil {
		rollback(tx)
		return nil, fmt.Errorf("set execution durability: %w", err)
	}
	return tx, nil
}
