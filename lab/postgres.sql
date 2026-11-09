-- Separate R24 acceptance: run against a disposable PostgreSQL database.
-- Replace the path below with a completed run's trace.jsonl; use an absolute
-- path and an account that can read it. No PostgreSQL driver is in the router.
-- Example invocation: psql "$GOODNEWS_DATABASE_URL" -f lab/postgres.sql
\set ON_ERROR_STOP on
BEGIN;
CREATE TEMP TABLE goodnews_trace (record jsonb);
-- COPY text escaping would reinterpret JSON backslashes. CSV with a control
-- delimiter and quote preserves each compact JSON line as exactly one value.
\copy goodnews_trace(record) FROM '/absolute/path/to/trace.jsonl' WITH (FORMAT csv, DELIMITER E'\x01', QUOTE E'\x02');
SELECT record->>'action' AS action,
       record->>'drop_reason' AS drop_reason,
       count(*) AS packets
FROM goodnews_trace
GROUP BY 1, 2
ORDER BY 1, 2;
ROLLBACK;
