-- Rollback for 000002_auth_billing_hardening

-- 24. Deployment integrity
DROP INDEX IF EXISTS idx_deployments_org_name;
DROP INDEX IF EXISTS idx_deployments_state_changed;
ALTER TABLE deployments DROP COLUMN IF EXISTS state_changed_at;
ALTER TABLE deployments DROP COLUMN IF EXISTS last_error;
ALTER TABLE deployments DROP COLUMN IF EXISTS artifact_id;

-- 23. BYO model ownership
DROP INDEX IF EXISTS idx_models_byo_name;
DROP INDEX IF EXISTS idx_models_catalog_name;
ALTER TABLE models ADD CONSTRAINT models_name_key UNIQUE (name);
ALTER TABLE models DROP CONSTRAINT IF EXISTS check_byo_has_owner;
DROP INDEX IF EXISTS idx_models_owner;
ALTER TABLE models DROP COLUMN IF EXISTS owner_org_id;

-- 22. Audit log
DROP INDEX IF EXISTS idx_audit_created;
DROP INDEX IF EXISTS idx_audit_request;
ALTER TABLE audit_log DROP COLUMN IF EXISTS user_agent;
ALTER TABLE audit_log DROP COLUMN IF EXISTS request_id;

-- 21. Model artifacts
DROP INDEX IF EXISTS idx_artifacts_verified;
ALTER TABLE model_artifacts DROP CONSTRAINT IF EXISTS check_verified_has_checksum;
ALTER TABLE model_artifacts DROP CONSTRAINT IF EXISTS check_real_checksum;
-- Restoring NOT NULL would fail on any row whose checksum is genuinely unknown,
-- so fill those with a marker first. Rolling back re-admits unverifiable rows;
-- that is the point of the rollback.
UPDATE model_artifacts SET checksum = 'sha256:unknown' WHERE checksum IS NULL;
ALTER TABLE model_artifacts ALTER COLUMN checksum SET NOT NULL;
ALTER TABLE model_artifacts DROP COLUMN IF EXISTS signature;
ALTER TABLE model_artifacts DROP COLUMN IF EXISTS verified_at;
ALTER TABLE model_artifacts DROP COLUMN IF EXISTS verified;

-- 20. Signup grants
DROP TABLE IF EXISTS signup_grants;

-- 19. Consents
DROP TABLE IF EXISTS user_consents;

-- 18. Data subject requests
DROP TABLE IF EXISTS data_subject_requests;

-- 17. Rate limits
DROP TABLE IF EXISTS rate_limits;

-- 16. Replica health
ALTER TABLE replicas DROP COLUMN IF EXISTS failed_requests;
ALTER TABLE replicas DROP COLUMN IF EXISTS total_requests;
ALTER TABLE replicas DROP COLUMN IF EXISTS active_requests;
ALTER TABLE replicas DROP COLUMN IF EXISTS healthy;
ALTER TABLE replicas DROP COLUMN IF EXISTS last_health_check_at;

-- 15. Host registration tokens
DROP TABLE IF EXISTS host_registration_tokens;

-- 14. Overlay IPs
DROP FUNCTION IF EXISTS allocate_overlay_ip(UUID);
DROP TABLE IF EXISTS overlay_ip_allocations;

-- 13. Host lifecycle
DROP INDEX IF EXISTS idx_hosts_heartbeat;
ALTER TABLE hosts ALTER COLUMN status SET DEFAULT 'registered';
ALTER TABLE hosts ALTER COLUMN kyc_status SET DEFAULT 'pending';
ALTER TABLE hosts DROP COLUMN IF EXISTS consecutive_failures;
ALTER TABLE hosts DROP COLUMN IF EXISTS probation_until;
ALTER TABLE hosts DROP COLUMN IF EXISTS benchmark_completed_at;
ALTER TABLE hosts DROP COLUMN IF EXISTS country_code;

-- 12. Uniqueness
DROP INDEX IF EXISTS idx_gpus_host_uuid;
DROP INDEX IF EXISTS idx_hosts_hw_fingerprint;

-- 11. Wallet ledger
DROP INDEX IF EXISTS idx_wallet_ledger_org_seq;
DROP INDEX IF EXISTS idx_wallet_ledger_seq;
ALTER TABLE wallet_ledger DROP COLUMN IF EXISTS currency;
ALTER TABLE wallet_ledger DROP COLUMN IF EXISTS seq;

-- 10. Payments
DROP TABLE IF EXISTS payout_accounts;
DROP TABLE IF EXISTS payment_webhook_events;
DROP TABLE IF EXISTS payment_intents;

-- 9. Wallet settings
DROP TABLE IF EXISTS wallet_settings;

-- 8. Invoices
ALTER TABLE invoices DROP COLUMN IF EXISTS reverse_charge;
ALTER TABLE invoices DROP COLUMN IF EXISTS tax_country;
ALTER TABLE invoices DROP COLUMN IF EXISTS tax_rate;
ALTER TABLE invoices DROP COLUMN IF EXISTS tax_name;
ALTER TABLE invoices DROP COLUMN IF EXISTS fx_rate;
ALTER TABLE invoices DROP COLUMN IF EXISTS currency;

-- 7. Organizations
ALTER TABLE organizations DROP COLUMN IF EXISTS is_business;
ALTER TABLE organizations DROP COLUMN IF EXISTS tax_id_enc;
ALTER TABLE organizations DROP COLUMN IF EXISTS currency;
ALTER TABLE organizations DROP COLUMN IF EXISTS billing_region;
ALTER TABLE organizations DROP COLUMN IF EXISTS billing_country;

-- 6. Tax jurisdictions
DROP TABLE IF EXISTS tax_jurisdictions;

-- 5. Regions: restore the original two-region CHECK constraints.
ALTER TABLE deployments DROP CONSTRAINT IF EXISTS fk_deployments_region;
ALTER TABLE organizations DROP CONSTRAINT IF EXISTS fk_orgs_region;
ALTER TABLE hosts DROP CONSTRAINT IF EXISTS fk_hosts_region;

DELETE FROM hosts WHERE region NOT IN ('IN-SOUTH', 'IN-WEST');
UPDATE organizations SET default_region = 'IN-SOUTH' WHERE default_region NOT IN ('IN-SOUTH', 'IN-WEST');

ALTER TABLE hosts ADD CONSTRAINT hosts_region_check
    CHECK (region IN ('IN-SOUTH', 'IN-WEST'));
ALTER TABLE organizations ADD CONSTRAINT organizations_default_region_check
    CHECK (default_region IN ('IN-SOUTH', 'IN-WEST'));

DROP TABLE IF EXISTS regions;

-- 4. Partition management
DROP FUNCTION IF EXISTS maintain_usage_events_partitions(INT);
DROP FUNCTION IF EXISTS ensure_usage_events_partition(DATE);

-- 3. Usage event idempotency indexes
DROP INDEX IF EXISTS idx_usage_events_2026_10_request;
DROP INDEX IF EXISTS idx_usage_events_2026_09_request;
DROP INDEX IF EXISTS idx_usage_events_2026_08_request;

-- 2. Auth tokens
DROP TABLE IF EXISTS auth_tokens;

-- 1. Revocation
DROP TABLE IF EXISTS token_invalidations;
DROP TABLE IF EXISTS revoked_tokens;
