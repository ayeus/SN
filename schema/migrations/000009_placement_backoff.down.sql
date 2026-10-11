DROP INDEX IF EXISTS idx_replicas_start_failures;
ALTER TABLE hosts
    DROP COLUMN IF EXISTS clock_skew_seconds,
    DROP COLUMN IF EXISTS last_error_at,
    DROP COLUMN IF EXISTS last_error;
ALTER TABLE deployments
    DROP COLUMN IF EXISTS placing_since;
