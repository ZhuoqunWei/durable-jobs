-- NULL preserves pre-existing jobs and submissions without a key. PostgreSQL's
-- ordinary UNIQUE constraint permits multiple NULLs, but only one of each key.
ALTER TABLE jobs
    ADD COLUMN idempotency_key text COLLATE "C",
    ADD CONSTRAINT jobs_idempotency_key_format CHECK (
        idempotency_key IS NULL OR
        idempotency_key ~ '^[A-Za-z0-9._:-]{1,128}$'
    ),
    ADD CONSTRAINT jobs_idempotency_key_unique UNIQUE (idempotency_key);
