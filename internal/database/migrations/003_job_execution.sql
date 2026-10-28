-- Existing queued jobs retain their IDs, payloads, keys and creation timestamps.
ALTER TABLE jobs
    DROP CONSTRAINT jobs_status_check,
    ADD COLUMN attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN started_at timestamptz,
    ADD COLUMN completed_at timestamptz,
    ADD COLUMN lease_token uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN result jsonb,
    ADD COLUMN failure_code text,
    ADD CONSTRAINT jobs_status_check CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    ADD CONSTRAINT jobs_execution_shape CHECK (
        (status = 'queued' AND attempts = 0 AND started_at IS NULL AND completed_at IS NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NULL AND failure_code IS NULL)
        OR
        (status = 'running' AND attempts = 1 AND started_at IS NOT NULL AND completed_at IS NULL
            AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL AND lease_expires_at > started_at
            AND result IS NULL AND failure_code IS NULL)
        OR
        (status = 'succeeded' AND attempts = 1 AND started_at IS NOT NULL AND completed_at IS NOT NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NOT NULL
            AND jsonb_typeof(result) = 'object' AND failure_code IS NULL)
        OR
        (status = 'failed' AND attempts = 1 AND started_at IS NOT NULL AND completed_at IS NOT NULL
            AND lease_token IS NULL AND lease_expires_at IS NULL AND result IS NULL
            AND failure_code IS NOT NULL
            AND failure_code IN ('invalid_campaign', 'unsupported_job_type', 'processing_timeout', 'processing_failed'))
    );

CREATE INDEX jobs_queued_order ON jobs (created_at, id) WHERE status = 'queued';
