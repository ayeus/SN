DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS gpu_skus;
DROP TABLE IF EXISTS fx_rates;
ALTER TABLE invoices RENAME COLUMN tax_amount TO gst_amount;
DROP INDEX IF EXISTS idx_usage_org_ts;
ALTER TABLE usage_events
    DROP COLUMN IF EXISTS fx_rate,
    DROP COLUMN IF EXISTS currency,
    DROP COLUMN IF EXISTS duration_ms,
    DROP COLUMN IF EXISTS model_id,
    DROP COLUMN IF EXISTS org_id;
DROP TABLE IF EXISTS deployment_events;
ALTER TABLE deployments DROP COLUMN IF EXISTS desired_state;
ALTER TABLE gpus DROP COLUMN IF EXISTS replica_id;
DROP INDEX IF EXISTS idx_replicas_state;
ALTER TABLE replicas
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS detail,
    DROP COLUMN IF EXISTS stop_sent_at,
    DROP COLUMN IF EXISTS dispatched_at;
DROP INDEX IF EXISTS idx_hosts_user;
DROP INDEX IF EXISTS idx_hosts_credential;
ALTER TABLE hosts
    DROP COLUMN IF EXISTS last_seen_ip,
    DROP COLUMN IF EXISTS paused,
    DROP COLUMN IF EXISTS cached_models,
    DROP COLUMN IF EXISTS runtime_healthy,
    DROP COLUMN IF EXISTS runtime,
    DROP COLUMN IF EXISTS os,
    DROP COLUMN IF EXISTS org_id,
    DROP COLUMN IF EXISTS credential_hash;
ALTER TABLE models
    DROP COLUMN IF EXISTS context_length,
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS runtime_refs;
