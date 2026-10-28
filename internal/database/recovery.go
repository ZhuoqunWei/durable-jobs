package database

import (
	"context"
	"errors"
	"fmt"

	"durable-jobs/internal/jobs"
	"github.com/jackc/pgx/v5"
)

// RecoverExpired closes at most limit abandoned attempts. It schedules a delayed
// retry or terminal failure; it never executes work or consumes another attempt.
// Every path locks jobs before job_attempts to keep a consistent lock order.
func (s *JobStore) RecoverExpired(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("recovery batch must contain 1–100 jobs")
	}
	tx, err := s.executionTransaction(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	rows, err := tx.Query(ctx, `SELECT `+jobColumns+`, lease_token::text FROM jobs
		WHERE status='running' AND lease_expires_at <= clock_timestamp()
		ORDER BY lease_expires_at,id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("find expired claims: %w", err)
	}
	var expired []jobs.Claim
	for rows.Next() {
		var claim jobs.Claim
		if err := rows.Scan(append(jobDestinations(&claim.Job), &claim.Token)...); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, claim)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, claim := range expired {
		next, delay := jobs.AfterFailure(jobs.LeaseExpired, claim.Job.Attempts, claim.Job.MaxAttempts)
		tag, err := tx.Exec(ctx, `UPDATE jobs SET status=$3, failure_code='lease_expired',
			completed_at=CASE WHEN $3='failed' THEN clock_timestamp() ELSE NULL END,
			available_at=CASE WHEN $3='retry_waiting' THEN clock_timestamp()+$4::bigint*interval '1 microsecond' ELSE available_at END,
			lease_token=NULL, lease_expires_at=NULL
			WHERE id=$1 AND status='running' AND lease_token::text=$2 AND lease_expires_at <= clock_timestamp()`,
			claim.Job.ID, claim.Token, next, delay.Microseconds())
		if err != nil {
			return 0, fmt.Errorf("recover expired claim: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, jobs.ErrLeaseLost
		}
		if err := recordAttemptEnd(ctx, tx, claim.Job.ID, claim.Job.Attempts, claim.Token, jobs.LeaseExpired, jobs.LeaseExpired); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit recovery (outcome may be unknown): %w", err)
	}
	return len(expired), nil
}

func recordAttemptEnd(ctx context.Context, tx pgx.Tx, id string, number int, token, status, code string) error {
	tag, err := tx.Exec(ctx, `UPDATE job_attempts SET status=$4, failure_code=NULLIF($5,''),
		finished_at=clock_timestamp(), lease_token=NULL
		WHERE job_id=$1 AND number=$2 AND lease_token::text=$3 AND status='running'`, id, number, token, status, code)
	if err != nil {
		return fmt.Errorf("record attempt outcome: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("active attempt history does not match the job claim")
	}
	return nil
}

func (s *JobStore) ListAttempts(ctx context.Context, id string) ([]jobs.Attempt, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1)", id).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, jobs.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT number,status,started_at,finished_at,lease_expires_at,COALESCE(failure_code,'')
		FROM job_attempts WHERE job_id=$1 ORDER BY number`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := make([]jobs.Attempt, 0)
	for rows.Next() {
		var attempt jobs.Attempt
		if err := rows.Scan(&attempt.Number, &attempt.Status, &attempt.StartedAt, &attempt.FinishedAt, &attempt.LeaseExpiresAt, &attempt.FailureCode); err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}
