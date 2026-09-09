-- AyeusANN Schema v2: Auth, billing, and globalization hardening
-- Migration: 000002_auth_billing_hardening
--
-- This migration addresses correctness and security defects in v1:
--   1. Token revocation was impossible (stateless JWTs, no denylist)
--   2. usage_events idempotency was broken: ON CONFLICT (request_id, ts) can
--      never fire because ts defaults to NOW(), so retries double-billed
--   3. Regions were CHECK-constrained to two Indian regions, so serving any
--      other country required a schema change
--   4. Tax was hardcoded at 18% GST for every customer regardless of country
--   5. Money had no currency attached
--   6. Wallet balances could go arbitrarily negative
--   7. Coordinator host upserts never matched, duplicating a host per restart

-- ─── 1. Token revocation ─────────────────────────────────────

CREATE TABLE revoked_tokens (
    jti         UUID PRIMARY KEY,
    expires_at  TIMESTAMPTZ NOT NULL,
    reason      TEXT NOT NULL DEFAULT 'revoked',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Purge job scans by expiry.
CREATE INDEX idx_revoked_tokens_expiry ON revoked_tokens (expires_at);

COMMENT ON TABLE revoked_tokens IS
    'Denylist of token IDs. Also used to consume single-use host registration tokens: the primary key makes redemption atomic.';

-- User-wide invalidation cutoff, for "sign out everywhere" and forced rotation
-- after a password change.
CREATE TABLE token_invalidations (
    user_id         UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    invalidated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reason          TEXT NOT NULL DEFAULT 'user_requested'
);

-- ─── 2. Email verification and password reset ────────────────

CREATE TABLE auth_tokens (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('email_verification', 'password_reset')),
    token_hash  TEXT NOT NULL UNIQUE,  -- SHA-256; the plaintext is only ever emailed
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_auth_tokens_user ON auth_tokens (user_id, kind) WHERE consumed_at IS NULL;
CREATE INDEX idx_auth_tokens_expiry ON auth_tokens (expires_at);

-- ─── 3. Fix usage_events idempotency ─────────────────────────

-- The v1 primary key (request_id, ts) allowed the same request_id to be inserted
-- repeatedly with different timestamps, so ON CONFLICT DO NOTHING never fired and
-- a retried usage event debited the customer twice.
--
-- Postgres requires the partition key in any unique constraint, so (request_id, ts)
-- must stay. Uniqueness on request_id alone is instead enforced per partition by a
-- unique index, which is sound because a single request_id is only ever written
-- once and therefore lands in exactly one partition.

CREATE UNIQUE INDEX idx_usage_events_2026_08_request
    ON usage_events_2026_08 (request_id);
CREATE UNIQUE INDEX idx_usage_events_2026_09_request
    ON usage_events_2026_09 (request_id);
CREATE UNIQUE INDEX idx_usage_events_2026_10_request
    ON usage_events_2026_10 (request_id);

-- ─── 4. Automatic partition management ───────────────────────

-- v1 hardcoded partitions through 2026-10-31 with a comment saying "in production,
-- a cron job creates future partitions". No such job existed, so every insert
-- would have failed from 2026-11-01 onward.

CREATE OR REPLACE FUNCTION ensure_usage_events_partition(target_month DATE)
RETURNS TEXT AS $$
DECLARE
    partition_start DATE := DATE_TRUNC('month', target_month)::DATE;
    partition_end   DATE := (DATE_TRUNC('month', target_month) + INTERVAL '1 month')::DATE;
    partition_name  TEXT := 'usage_events_' || TO_CHAR(partition_start, 'YYYY_MM');
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = partition_name) THEN
        RETURN partition_name || ' (exists)';
    END IF;

    EXECUTE FORMAT(
        'CREATE TABLE %I PARTITION OF usage_events FOR VALUES FROM (%L) TO (%L);',
        partition_name, partition_start, partition_end
    );

    -- Per-partition idempotency guard, matching the indexes above.
    EXECUTE FORMAT(
        'CREATE UNIQUE INDEX %I ON %I (request_id);',
        'idx_' || partition_name || '_request', partition_name
    );

    RETURN partition_name || ' (created)';
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION ensure_usage_events_partition IS
    'Idempotently creates the usage_events partition for the given month. Call from the partition maintenance job.';

-- Maintain a rolling window of partitions ahead of the current month so that
-- ingestion never hits a missing partition even if the job is delayed.
CREATE OR REPLACE FUNCTION maintain_usage_events_partitions(months_ahead INT DEFAULT 3)
RETURNS SETOF TEXT AS $$
DECLARE
    i INT;
BEGIN
    FOR i IN 0..months_ahead LOOP
        RETURN NEXT ensure_usage_events_partition((CURRENT_DATE + (i || ' month')::INTERVAL)::DATE);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- Create the window immediately so the deployed system is not relying on the
-- job's first run.
SELECT maintain_usage_events_partitions(6);

-- ─── 5. Regions: table instead of CHECK constraint ───────────

CREATE TABLE regions (
    code            TEXT PRIMARY KEY,
    display_name    TEXT NOT NULL,
    country_code    CHAR(2) NOT NULL,   -- ISO 3166-1 alpha-2
    continent       TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO regions (code, display_name, country_code, continent) VALUES
    ('IN-SOUTH', 'India South (Chennai)',        'IN', 'Asia'),
    ('IN-WEST',  'India West (Mumbai)',          'IN', 'Asia'),
    ('US-EAST',  'US East (N. Virginia)',        'US', 'North America'),
    ('US-WEST',  'US West (Oregon)',             'US', 'North America'),
    ('EU-WEST',  'Europe West (Ireland)',        'IE', 'Europe'),
    ('EU-CENTRAL','Europe Central (Frankfurt)',  'DE', 'Europe'),
    ('UK-SOUTH', 'UK South (London)',            'GB', 'Europe'),
    ('AP-SOUTH', 'Asia Pacific (Singapore)',     'SG', 'Asia'),
    ('AP-NORTHEAST', 'Asia Pacific (Tokyo)',     'JP', 'Asia'),
    ('AP-SOUTHEAST', 'Asia Pacific (Sydney)',    'AU', 'Oceania'),
    ('SA-EAST',  'South America East (São Paulo)','BR', 'South America'),
    ('CA-CENTRAL','Canada Central (Toronto)',    'CA', 'North America'),
    ('ME-CENTRAL','Middle East (Dubai)',         'AE', 'Asia'),
    ('AF-SOUTH', 'Africa South (Cape Town)',     'ZA', 'Africa');

-- Replace the two-region CHECK constraints with foreign keys.
ALTER TABLE hosts DROP CONSTRAINT IF EXISTS hosts_region_check;
ALTER TABLE hosts
    ADD CONSTRAINT fk_hosts_region FOREIGN KEY (region) REFERENCES regions(code);

ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_default_region_check;
ALTER TABLE organizations
    ADD CONSTRAINT fk_orgs_region FOREIGN KEY (default_region) REFERENCES regions(code);

ALTER TABLE deployments
    ADD CONSTRAINT fk_deployments_region FOREIGN KEY (region) REFERENCES regions(code);

-- ─── 6. Tax jurisdictions ────────────────────────────────────

-- v1 applied GSTRate = 0.18 to every invoice unconditionally, which is wrong for
-- every customer outside India.
CREATE TABLE tax_jurisdictions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    country_code    CHAR(2) NOT NULL,
    region_code     TEXT,               -- state/province, NULL = country-wide
    tax_name        TEXT NOT NULL,      -- GST, VAT, Sales Tax, ...
    rate            NUMERIC(6,4) NOT NULL CHECK (rate >= 0 AND rate <= 1),
    reverse_charge  BOOLEAN NOT NULL DEFAULT FALSE,  -- B2B cross-border: customer self-accounts
    tax_id_required BOOLEAN NOT NULL DEFAULT FALSE,
    effective_from  DATE NOT NULL DEFAULT CURRENT_DATE,
    effective_to    DATE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (country_code, region_code, effective_from)
);

CREATE INDEX idx_tax_jurisdictions_lookup
    ON tax_jurisdictions (country_code, effective_from DESC);

INSERT INTO tax_jurisdictions (country_code, tax_name, rate, tax_id_required) VALUES
    ('IN', 'GST',       0.1800, TRUE),
    ('GB', 'VAT',       0.2000, FALSE),
    ('DE', 'VAT',       0.1900, FALSE),
    ('IE', 'VAT',       0.2300, FALSE),
    ('FR', 'VAT',       0.2000, FALSE),
    ('NL', 'VAT',       0.2100, FALSE),
    ('ES', 'VAT',       0.2100, FALSE),
    ('IT', 'VAT',       0.2200, FALSE),
    ('AU', 'GST',       0.1000, FALSE),
    ('NZ', 'GST',       0.1500, FALSE),
    ('SG', 'GST',       0.0900, FALSE),
    ('JP', 'CT',        0.1000, FALSE),
    ('CA', 'GST',       0.0500, FALSE),
    ('BR', 'ISS',       0.0500, FALSE),
    ('ZA', 'VAT',       0.1500, FALSE),
    ('AE', 'VAT',       0.0500, FALSE),
    ('US', 'Sales Tax', 0.0000, FALSE);  -- Nexus-dependent; resolved per state

COMMENT ON TABLE tax_jurisdictions IS
    'Tax rates by country. Rates change by legislation; this table is data, not code. US sales tax is nexus-dependent and defaults to zero pending per-state configuration.';

-- ─── 7. Currency and billing address on organizations ────────

ALTER TABLE organizations
    ADD COLUMN billing_country CHAR(2),
    ADD COLUMN billing_region  TEXT,
    ADD COLUMN currency        CHAR(3) NOT NULL DEFAULT 'USD',
    ADD COLUMN tax_id_enc      BYTEA,  -- VAT/GST number, encrypted at the application layer
    ADD COLUMN is_business     BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN organizations.currency IS
    'ISO 4217 billing currency. All internal amounts are USD; conversion happens at invoice time using a recorded fx_rate.';

-- Existing organizations were implicitly Indian.
UPDATE organizations SET billing_country = 'IN', currency = 'INR' WHERE billing_country IS NULL;

-- ─── 8. Invoices: currency, FX, and general tax ──────────────

ALTER TABLE invoices
    ADD COLUMN currency      CHAR(3) NOT NULL DEFAULT 'USD',
    ADD COLUMN fx_rate       NUMERIC(18,8) NOT NULL DEFAULT 1.0,
    ADD COLUMN tax_name      TEXT NOT NULL DEFAULT 'GST',
    ADD COLUMN tax_rate      NUMERIC(6,4) NOT NULL DEFAULT 0,
    ADD COLUMN tax_country   CHAR(2),
    ADD COLUMN reverse_charge BOOLEAN NOT NULL DEFAULT FALSE;

-- gst_amount is now the generic tax amount; the column name is retained to avoid
-- breaking existing readers, with tax_name recording what the amount represents.
COMMENT ON COLUMN invoices.gst_amount IS
    'Tax amount. Despite the name this holds whichever tax applied; see tax_name and tax_rate.';
COMMENT ON COLUMN invoices.fx_rate IS
    'USD to invoice currency rate at issue time. Recorded so a historical invoice can always be reproduced exactly.';

-- ─── 9. Wallet: credit limits instead of unbounded negatives ─

CREATE TABLE wallet_settings (
    org_id              UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    credit_limit        NUMERIC(14,6) NOT NULL DEFAULT 0 CHECK (credit_limit >= 0),
    low_balance_threshold NUMERIC(14,6) NOT NULL DEFAULT 5.0,
    auto_topup_enabled  BOOLEAN NOT NULL DEFAULT FALSE,
    auto_topup_amount   NUMERIC(14,6),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE wallet_settings IS
    'Per-org overdraft allowance. v1 passed allowNegativeBalance=true on every debit, so balances could fall arbitrarily negative with no collection path.';

-- ─── 10. Payments ────────────────────────────────────────────

CREATE TABLE payment_intents (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id              UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    provider            TEXT NOT NULL CHECK (provider IN ('stripe', 'razorpay')),
    provider_intent_id  TEXT NOT NULL,
    amount              NUMERIC(14,6) NOT NULL CHECK (amount > 0),
    currency            CHAR(3) NOT NULL,
    amount_usd          NUMERIC(14,6) NOT NULL,
    fx_rate             NUMERIC(18,8) NOT NULL DEFAULT 1.0,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'processing', 'succeeded', 'failed', 'cancelled', 'refunded')),
    ledger_entry_id     UUID REFERENCES wallet_ledger(entry_id),
    idempotency_key     TEXT NOT NULL UNIQUE,
    failure_reason      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_intent_id)
);

CREATE INDEX idx_payment_intents_org ON payment_intents (org_id, created_at DESC);
CREATE INDEX idx_payment_intents_status ON payment_intents (status) WHERE status IN ('pending', 'processing');

-- Webhooks must be idempotent: providers retry, and a replayed top-up event
-- would otherwise credit the wallet twice.
CREATE TABLE payment_webhook_events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider        TEXT NOT NULL,
    provider_event_id TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL,
    processed_at    TIMESTAMPTZ,
    error           TEXT,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_event_id)
);

CREATE INDEX idx_webhook_events_unprocessed
    ON payment_webhook_events (received_at) WHERE processed_at IS NULL;

-- Host payout destinations. v1 modelled payout batches but had nowhere to send money.
CREATE TABLE payout_accounts (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    provider        TEXT NOT NULL CHECK (provider IN ('stripe_connect', 'razorpayx', 'wise')),
    provider_account_id TEXT NOT NULL,
    country_code    CHAR(2) NOT NULL,
    currency        CHAR(3) NOT NULL,
    account_enc     BYTEA,   -- Encrypted account details
    verified        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (host_user_id, provider)
);

-- ─── 11. Wallet ledger: currency and ordering ────────────────

-- v1 ordered the ledger by (created_at DESC, entry_id DESC) where entry_id is a
-- random UUIDv4, so two entries sharing a timestamp had nondeterministic order
-- and "the latest balance" was ambiguous. A monotonic sequence fixes the ordering.
ALTER TABLE wallet_ledger
    ADD COLUMN seq BIGSERIAL,
    ADD COLUMN currency CHAR(3) NOT NULL DEFAULT 'USD';

CREATE UNIQUE INDEX idx_wallet_ledger_seq ON wallet_ledger (seq);
CREATE INDEX idx_wallet_ledger_org_seq ON wallet_ledger (org_id, seq DESC);

-- ─── 12. Host uniqueness ─────────────────────────────────────

-- The coordinator's ON CONFLICT (id) never matched because id is generated, so
-- each agent restart inserted a duplicate host. A host is identified by its
-- hardware fingerprint.
DELETE FROM hosts h
USING hosts dup
WHERE h.hw_fingerprint = dup.hw_fingerprint
  AND h.hw_fingerprint IS NOT NULL
  AND h.created_at < dup.created_at;

CREATE UNIQUE INDEX idx_hosts_hw_fingerprint
    ON hosts (hw_fingerprint)
    WHERE hw_fingerprint IS NOT NULL;

-- Same defect for GPUs: ON CONFLICT DO NOTHING with no unique constraint to
-- conflict against, so GPUs duplicated on every re-registration.
DELETE FROM gpus g
USING gpus dup
WHERE g.host_id = dup.host_id
  AND g.uuid = dup.uuid
  AND g.created_at < dup.created_at;

CREATE UNIQUE INDEX idx_gpus_host_uuid ON gpus (host_id, uuid);

-- ─── 13. Host lifecycle and heartbeats ───────────────────────

ALTER TABLE hosts
    ADD COLUMN country_code CHAR(2),
    ADD COLUMN benchmark_completed_at TIMESTAMPTZ,
    ADD COLUMN probation_until TIMESTAMPTZ,
    ADD COLUMN consecutive_failures INT NOT NULL DEFAULT 0;

-- Registration must not self-certify KYC. v1 inserted kyc_status='verified' and
-- status='active' directly, bypassing the entire documented state machine.
ALTER TABLE hosts ALTER COLUMN kyc_status SET DEFAULT 'pending';
ALTER TABLE hosts ALTER COLUMN status SET DEFAULT 'registered';

CREATE INDEX idx_hosts_heartbeat ON hosts (last_heartbeat_at)
    WHERE status = 'active' AND deleted_at IS NULL;

-- ─── 14. Overlay IP allocation ───────────────────────────────

-- v1 allocated overlay IPs from an in-memory counter starting at 10, so IPs
-- collided after every coordinator restart and across replicas.
CREATE TABLE overlay_ip_allocations (
    ip          INET PRIMARY KEY,
    host_id     UUID REFERENCES hosts(id) ON DELETE SET NULL,
    allocated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at TIMESTAMPTZ
);

CREATE INDEX idx_overlay_ip_host ON overlay_ip_allocations (host_id) WHERE released_at IS NULL;

CREATE OR REPLACE FUNCTION allocate_overlay_ip(p_host_id UUID)
RETURNS INET AS $$
DECLARE
    existing INET;
    candidate INET;
    octet3 INT;
    octet4 INT;
BEGIN
    -- Re-registration keeps the previously assigned address.
    SELECT ip INTO existing
    FROM overlay_ip_allocations
    WHERE host_id = p_host_id AND released_at IS NULL
    LIMIT 1;

    IF existing IS NOT NULL THEN
        RETURN existing;
    END IF;

    -- 10.200.0.0/16, reserving 10.200.0.1 for the coordinator endpoint.
    FOR octet3 IN 0..255 LOOP
        FOR octet4 IN 2..254 LOOP
            candidate := ('10.200.' || octet3 || '.' || octet4)::INET;
            BEGIN
                INSERT INTO overlay_ip_allocations (ip, host_id) VALUES (candidate, p_host_id);
                RETURN candidate;
            EXCEPTION WHEN unique_violation THEN
                -- Address taken; keep scanning.
            END;
        END LOOP;
    END LOOP;

    RAISE EXCEPTION 'overlay IP pool 10.200.0.0/16 exhausted';
END;
$$ LANGUAGE plpgsql;

-- ─── 15. Host registration tokens ────────────────────────────

-- Records issued registration tokens so they can be listed, revoked before use,
-- and audited. Single-use enforcement is handled by revoked_tokens.
CREATE TABLE host_registration_tokens (
    jti         UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    label       TEXT,
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_by_host UUID REFERENCES hosts(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_host_reg_tokens_user ON host_registration_tokens (user_id, created_at DESC);

-- ─── 16. Replica heartbeats for real health checking ─────────

ALTER TABLE replicas
    ADD COLUMN last_health_check_at TIMESTAMPTZ,
    ADD COLUMN healthy BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN active_requests INT NOT NULL DEFAULT 0,
    ADD COLUMN total_requests BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN failed_requests BIGINT NOT NULL DEFAULT 0;

-- The router previously treated unknown replicas as healthy.
COMMENT ON COLUMN replicas.healthy IS
    'Set by the router health checker. Defaults to false so an unprobed replica is never sent traffic.';

-- ─── 17. Rate limit configuration ────────────────────────────

CREATE TABLE rate_limits (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    scope           TEXT NOT NULL CHECK (scope IN ('org', 'api_key', 'ip', 'global')),
    scope_id        TEXT,
    requests_per_min INT NOT NULL DEFAULT 60,
    tokens_per_min  INT,
    concurrent_requests INT NOT NULL DEFAULT 10,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (scope, scope_id)
);

INSERT INTO rate_limits (scope, scope_id, requests_per_min, concurrent_requests)
VALUES ('global', NULL, 10000, 1000);

-- ─── 18. GDPR / DPDP data subject requests ───────────────────

CREATE TABLE data_subject_requests (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('access', 'erasure', 'portability', 'rectification')),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processing', 'completed', 'rejected')),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- GDPR Art. 12(3): respond within one month.
    due_at          TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),
    completed_at    TIMESTAMPTZ,
    export_url      TEXT,
    notes           TEXT
);

CREATE INDEX idx_dsr_status ON data_subject_requests (status, due_at) WHERE status IN ('pending', 'processing');

COMMENT ON TABLE data_subject_requests IS
    'GDPR and India DPDP data subject requests. Required before serving EU or Indian users at scale.';

-- ─── 19. Consent records ─────────────────────────────────────

CREATE TABLE user_consents (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('terms', 'privacy', 'marketing', 'data_processing')),
    version     TEXT NOT NULL,
    granted     BOOLEAN NOT NULL,
    ip_address  INET,
    user_agent  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_user_consents_user ON user_consents (user_id, kind, created_at DESC);

-- ─── 20. Signup abuse controls ───────────────────────────────

-- v1 granted a $50 promotional credit on every signup with no throttle, which is
-- trivially farmable.
CREATE TABLE signup_grants (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount          NUMERIC(14,6) NOT NULL,
    email_domain    TEXT NOT NULL,
    signup_ip       INET,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_signup_grants_ip ON signup_grants (signup_ip, created_at DESC);
CREATE INDEX idx_signup_grants_domain ON signup_grants (email_domain, created_at DESC);
CREATE UNIQUE INDEX idx_signup_grants_user ON signup_grants (user_id);

-- ─── 21. Model artifact verification ─────────────────────────

ALTER TABLE model_artifacts
    ADD COLUMN verified BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN verified_at TIMESTAMPTZ,
    ADD COLUMN signature TEXT;

-- checksum was NOT NULL, which forced every caller to invent one; the seed
-- shipped 'sha256:placeholder_will_be_updated_with_real_checksum' and the BYO
-- endpoint wrote 'sha256:byo_huggingface'. A value that is not a hash must not
-- occupy a column that a download path compares against, so the column is now
-- nullable and the absence of a hash is representable.
ALTER TABLE model_artifacts
    ALTER COLUMN checksum DROP NOT NULL;

-- Drop any artifact rows carrying an invented checksum. They describe nothing
-- that can be verified, and a deployment must not be able to trust them.
DELETE FROM model_artifacts
 WHERE checksum IS NULL
    OR checksum NOT SIMILAR TO 'sha256:[0-9a-f]{64}';

-- A checksum, when present, must be a real lowercase hex SHA-256 digest.
ALTER TABLE model_artifacts
    ADD CONSTRAINT check_real_checksum
    CHECK (checksum IS NULL OR checksum SIMILAR TO 'sha256:[0-9a-f]{64}');

-- Verification is only meaningful with a digest to verify against.
ALTER TABLE model_artifacts
    ADD CONSTRAINT check_verified_has_checksum
    CHECK (NOT verified OR (checksum IS NOT NULL AND verified_at IS NOT NULL));

-- Only a verified artifact may be served to a host.
CREATE INDEX idx_artifacts_verified ON model_artifacts (model_id) WHERE verified;

-- ─── 22. Audit log retention ─────────────────────────────────

ALTER TABLE audit_log
    ADD COLUMN request_id UUID,
    ADD COLUMN user_agent TEXT;

CREATE INDEX idx_audit_request ON audit_log (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX idx_audit_created ON audit_log (created_at DESC);

-- ─── 23. BYO model ownership ─────────────────────────────────
-- models had no owner column, so a bring-your-own model registered by one
-- customer was returned to every other customer by GET /v1/models, along with
-- its name and its pricing. Catalogue models keep a NULL owner and stay public;
-- a BYO model belongs to exactly one organization.

ALTER TABLE models
    ADD COLUMN owner_org_id UUID REFERENCES organizations (id) ON DELETE CASCADE;

CREATE INDEX idx_models_owner ON models (owner_org_id) WHERE owner_org_id IS NOT NULL;

ALTER TABLE models
    ADD CONSTRAINT check_byo_has_owner
    CHECK ((is_byo AND owner_org_id IS NOT NULL) OR (NOT is_byo AND owner_org_id IS NULL));

-- Model names were globally unique, so one tenant registering "my-finetune"
-- permanently blocked the name for everyone else and learned, from the 409,
-- that another tenant already held it. Uniqueness now applies within the
-- catalogue and within each organization separately.
ALTER TABLE models DROP CONSTRAINT models_name_key;

CREATE UNIQUE INDEX idx_models_catalog_name
    ON models (name) WHERE owner_org_id IS NULL;

CREATE UNIQUE INDEX idx_models_byo_name
    ON models (owner_org_id, name) WHERE owner_org_id IS NOT NULL;

-- ─── 24. Deployment integrity ────────────────────────────────
-- A deployment referenced a model with no check that the org may use it, and
-- endpoints were free text. Both are enforced here rather than in application
-- code alone.

ALTER TABLE deployments
    ADD COLUMN artifact_id UUID REFERENCES model_artifacts (id) ON DELETE RESTRICT,
    ADD COLUMN last_error TEXT,
    ADD COLUMN state_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX idx_deployments_state_changed ON deployments (state, state_changed_at DESC);

-- Deployment names are how customers address their endpoints, so they must be
-- unique inside an organization.
CREATE UNIQUE INDEX idx_deployments_org_name
    ON deployments (org_id, name) WHERE deleted_at IS NULL;
