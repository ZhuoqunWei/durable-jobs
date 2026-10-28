-- Fresh, synthetic lab database only. The official image invokes this once.
-- psql quoting handles punctuation; no password is substituted into source.
\getenv app_password APP_PASSWORD
CREATE ROLE jobs_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
    NOREPLICATION PASSWORD :'app_password';
ALTER DATABASE durable_jobs OWNER TO jobs_owner;
REVOKE ALL ON DATABASE durable_jobs FROM PUBLIC;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO jobs_owner;
-- API/worker/migration share this non-superuser schema owner in this lab.
-- Separate a DML-only application role before claiming production least privilege.
