-- Preserve every pre-existing job value. Terminal jobs remain terminal.
ALTER TABLE jobs
    DROP CONSTRAINT jobs_status_check,
    DROP CONSTRAINT jobs_execution_shape,
    ADD COLUMN max_attempts integer NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    ADD COLUMN available_at timestamptz NOT NULL DEFAULT now();

UPDATE jobs SET available_at = created_at;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_attempt_budget CHECK (attempts BETWEEN 0 AND max_attempts),
    ADD CONSTRAINT jobs_status_check CHECK (status IN ('queued','running','retry_waiting','succeeded','failed')),
    ADD CONSTRAINT jobs_execution_shape CHECK (
        (status = 'queued' AND attempts = 0 AND started_at IS NULL AND completed_at IS NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NULL AND failure_code IS NULL)
        OR
        (status = 'running' AND attempts >= 1 AND started_at IS NOT NULL AND completed_at IS NULL
            AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL AND lease_expires_at > started_at
            AND result IS NULL AND failure_code IS NULL)
        OR
        (status = 'retry_waiting' AND attempts >= 1 AND attempts < max_attempts
            AND started_at IS NOT NULL AND completed_at IS NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NULL
            AND failure_code IS NOT NULL AND failure_code IN ('processing_timeout','transient_failure','lease_expired'))
        OR
        (status = 'succeeded' AND attempts >= 1 AND started_at IS NOT NULL AND completed_at IS NOT NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NOT NULL
            AND jsonb_typeof(result) = 'object' AND failure_code IS NULL)
        OR
        (status = 'failed' AND attempts >= 1 AND started_at IS NOT NULL AND completed_at IS NOT NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NULL
            AND failure_code IS NOT NULL AND failure_code IN ('invalid_campaign','unsupported_job_type',
                'processing_timeout','processing_failed','transient_failure','lease_expired'))
    );

CREATE TABLE job_attempts (
    job_id uuid NOT NULL REFERENCES jobs(id),
    number integer NOT NULL CHECK (number BETWEEN 1 AND 10),
    status text NOT NULL CHECK (status IN ('running','succeeded','failed','lease_expired')),
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    lease_expires_at timestamptz,
    lease_token uuid,
    failure_code text,
    PRIMARY KEY(job_id, number),
    CHECK (
        (status = 'running' AND finished_at IS NULL AND failure_code IS NULL
            AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR
        (status = 'succeeded' AND finished_at IS NOT NULL AND failure_code IS NULL AND lease_token IS NULL)
        OR
        (status = 'failed' AND finished_at IS NOT NULL AND lease_token IS NULL
            AND failure_code IS NOT NULL AND failure_code IN ('invalid_campaign','unsupported_job_type',
                'processing_timeout','processing_failed','transient_failure'))
        OR
        (status = 'lease_expired' AND finished_at IS NOT NULL AND lease_token IS NULL
            AND failure_code IS NOT NULL AND failure_code = 'lease_expired')
    )
);

CREATE UNIQUE INDEX job_attempts_one_running ON job_attempts(job_id) WHERE status = 'running';
CREATE INDEX jobs_available_order ON jobs(available_at, created_at, id) WHERE status IN ('queued','retry_waiting');
CREATE INDEX jobs_expired_order ON jobs(lease_expires_at, id) WHERE status = 'running';

-- Version 003 allowed only one attempt. No fabricated retries or timestamps:
-- terminal attempts have NULL lease expiry because 003 cleared that field.
INSERT INTO job_attempts(job_id, number, status, started_at, finished_at, lease_expires_at, lease_token, failure_code)
SELECT id, attempts, status, started_at, completed_at, lease_expires_at, lease_token, failure_code
FROM jobs WHERE attempts > 0;
