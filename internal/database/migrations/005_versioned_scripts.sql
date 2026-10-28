-- Additive upgrade: no job values, attempt history or budgets are rewritten.
-- Stop old APIs/workers before applying; old workers do not know run_script.
ALTER TABLE jobs DROP CONSTRAINT jobs_type_check,
    ADD CONSTRAINT jobs_type_check CHECK (type IN ('migrate_campaign', 'run_script')),
    ADD CONSTRAINT jobs_script_shape CHECK (type <> 'run_script' OR (
        payload ?& ARRAY['script','version','source_sha256','image_id','input']
        AND jsonb_typeof(payload->'script') = 'string'
        AND jsonb_typeof(payload->'version') = 'string'
        AND jsonb_typeof(payload->'source_sha256') = 'string'
        AND jsonb_typeof(payload->'image_id') = 'string'
        AND jsonb_typeof(payload->'input') = 'object'
        AND length(payload->>'script') BETWEEN 1 AND 64
        AND length(payload->>'version') BETWEEN 1 AND 32
        AND (payload->>'source_sha256') ~ '^[0-9a-f]{64}$'
        AND (payload->>'image_id') ~ '^sha256:[0-9a-f]{64}$'
        AND (payload - ARRAY['script','version','source_sha256','image_id','input']) = '{}'::jsonb
    ));

ALTER TABLE jobs DROP CONSTRAINT jobs_execution_shape,
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
                'processing_timeout','processing_failed','transient_failure','lease_expired',
                'script_failed','script_limit_exceeded','script_version_unavailable','sandbox_cleanup_failed'))
    );

ALTER TABLE job_attempts DROP CONSTRAINT job_attempts_check,
    ADD CONSTRAINT job_attempts_check CHECK (
        (status = 'running' AND finished_at IS NULL AND failure_code IS NULL
            AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR
        (status = 'succeeded' AND finished_at IS NOT NULL AND failure_code IS NULL AND lease_token IS NULL)
        OR
        (status = 'failed' AND finished_at IS NOT NULL AND lease_token IS NULL
            AND failure_code IS NOT NULL AND failure_code IN ('invalid_campaign','unsupported_job_type',
                'processing_timeout','processing_failed','transient_failure',
                'script_failed','script_limit_exceeded','script_version_unavailable','sandbox_cleanup_failed'))
        OR
        (status = 'lease_expired' AND finished_at IS NOT NULL AND lease_token IS NULL
            AND failure_code IS NOT NULL AND failure_code = 'lease_expired')
    );
