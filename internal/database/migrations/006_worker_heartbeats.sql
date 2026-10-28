-- Observability only: no job fields or execution semantics are changed.
CREATE TABLE worker_heartbeats (
    worker_id text PRIMARY KEY CHECK (length(worker_id) BETWEEN 1 AND 128),
    seen_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
