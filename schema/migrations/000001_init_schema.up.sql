-- SpazeNode Schema v1: Core Tables
-- Migration: 000001_init_schema
-- Description: Creates all core tables for the SpazeNode platform.
-- 
-- IMPORTANT INVARIANTS:
--   1. usage_events is APPEND-ONLY (no UPDATE/DELETE in application code)
--   2. wallet_ledger uses compensating entries (no destructive updates)
--   3. PII columns (pan_enc, bank_enc, phone_enc) are encrypted at application layer
--   4. All timestamps are UTC (TIMESTAMPTZ)
--   5. Soft-delete via deleted_at where indicated

-- Enable UUID generation
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ─── 1. Price Books ──────────────────────────────────────────

CREATE TABLE price_books (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        TEXT NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─── 2. Users ────────────────────────────────────────────────

CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT,  -- NULL for OAuth-only users
    name            TEXT NOT NULL,
    phone_enc       BYTEA,  -- Encrypted phone number
    auth_provider   TEXT NOT NULL DEFAULT 'email' CHECK (auth_provider IN ('email', 'google', 'github')),
    email_verified  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ  -- Soft delete
);

CREATE INDEX idx_users_email ON users (email) WHERE deleted_at IS NULL;

-- ─── 3. Organizations ────────────────────────────────────────

CREATE TABLE organizations (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name            TEXT NOT NULL,
    gstin_enc       BYTEA,  -- Encrypted GSTIN
    default_region  TEXT NOT NULL DEFAULT 'IN-SOUTH' CHECK (default_region IN ('IN-SOUTH', 'IN-WEST')),
    price_book_id   UUID REFERENCES price_books(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ
);

-- ─── 4. Memberships ──────────────────────────────────────────

CREATE TABLE memberships (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'member', 'billing')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, org_id)
);

CREATE INDEX idx_memberships_org ON memberships (org_id);
CREATE INDEX idx_memberships_user ON memberships (user_id);

-- ─── 5. API Keys ─────────────────────────────────────────────

CREATE TABLE api_keys (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    name            TEXT NOT NULL,
    prefix          TEXT NOT NULL,  -- First 8 chars for lookup (e.g., sk_live_a1b2)
    hash            TEXT NOT NULL,  -- SHA-256 of the full key
    scope           TEXT NOT NULL DEFAULT 'org' CHECK (scope IN ('org', 'deployment')),
    deployment_id   UUID,  -- FK added after deployments table
    permissions     JSONB NOT NULL DEFAULT '["*"]',
    last_used_at    TIMESTAMPTZ,
    revoked         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_api_keys_prefix ON api_keys (prefix) WHERE NOT revoked;
CREATE INDEX idx_api_keys_org ON api_keys (org_id);

-- ─── 6. Models ───────────────────────────────────────────────

CREATE TABLE models (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name                TEXT NOT NULL UNIQUE,
    family              TEXT NOT NULL,
    params_b            REAL NOT NULL,  -- Parameter count in billions
    license             TEXT NOT NULL,
    min_vram_gb         INT NOT NULL,
    tiers_allowed       TEXT[] NOT NULL DEFAULT '{t1,t2,t3}',
    price_in_per_1m     NUMERIC(12,4) NOT NULL,  -- Per 1M input tokens (USD)
    price_out_per_1m    NUMERIC(12,4) NOT NULL,  -- Per 1M output tokens (USD)
    price_per_hour_inr  NUMERIC(12,2),  -- Optional hourly rate in INR
    is_byo              BOOLEAN NOT NULL DEFAULT FALSE,
    quantization_presets JSONB NOT NULL DEFAULT '[]',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─── 7. Model Artifacts ──────────────────────────────────────

CREATE TABLE model_artifacts (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    model_id    UUID NOT NULL REFERENCES models(id) ON DELETE RESTRICT,
    version     TEXT NOT NULL,
    artifact_url TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL,
    checksum    TEXT NOT NULL,
    format      TEXT NOT NULL DEFAULT 'safetensors' CHECK (format IN ('safetensors', 'gguf', 'gptq', 'awq')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, version)
);

-- ─── 8. Hosts ────────────────────────────────────────────────

CREATE TABLE hosts (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id             UUID REFERENCES users(id) ON DELETE RESTRICT,
    name                TEXT NOT NULL,
    hostname            TEXT,
    tier                TEXT NOT NULL CHECK (tier IN ('t1', 't2', 't3')),
    region              TEXT NOT NULL DEFAULT 'IN-SOUTH' CHECK (region IN ('IN-SOUTH', 'IN-WEST')),
    overlay_ip          INET,
    kyc_status          TEXT NOT NULL DEFAULT 'pending' CHECK (kyc_status IN ('pending', 'submitted', 'verified', 'rejected')),
    reputation          INT NOT NULL DEFAULT 50 CHECK (reputation >= 0 AND reputation <= 100),
    status              TEXT NOT NULL DEFAULT 'registered' CHECK (status IN ('registered', 'benchmarking', 'probation', 'active', 'draining', 'offline', 'demoted', 'banned')),
    hw_fingerprint      TEXT,
    pan_enc             BYTEA,  -- Encrypted PAN
    bank_enc            BYTEA,  -- Encrypted bank details
    agent_version       TEXT,
    last_heartbeat_at   TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ
);

CREATE INDEX idx_hosts_status ON hosts (status) WHERE deleted_at IS NULL;
CREATE INDEX idx_hosts_region_tier ON hosts (region, tier) WHERE status = 'active' AND deleted_at IS NULL;

-- ─── 9. GPUs ─────────────────────────────────────────────────

CREATE TABLE gpus (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    model           TEXT NOT NULL,
    vram_gb         INT NOT NULL,
    driver_version  TEXT,
    cuda_version    TEXT,
    uuid            TEXT NOT NULL,  -- NVIDIA GPU UUID
    fingerprint     TEXT,
    status          TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'reserved', 'in_use', 'unavailable')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_gpus_host ON gpus (host_id);
CREATE INDEX idx_gpus_available ON gpus (status, vram_gb) WHERE status = 'available';

-- ─── 10. Benchmarks ──────────────────────────────────────────

CREATE TABLE benchmarks (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_id             UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    gpu_id              UUID REFERENCES gpus(id),
    score_compute       REAL NOT NULL,
    vram_bw_gbps        REAL,
    disk_read_mbps      REAL,
    disk_write_mbps     REAL,
    net_up_mbps         REAL,
    net_down_mbps       REAL,
    latency_pop_ms      REAL,
    hw_fingerprint      TEXT NOT NULL,
    ran_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_benchmarks_host ON benchmarks (host_id, ran_at DESC);

-- ─── 11. Reputation Snapshots ────────────────────────────────

CREATE TABLE reputation_snapshots (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_id             UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    score               INT NOT NULL CHECK (score >= 0 AND score <= 100),
    uptime_pct          REAL NOT NULL,
    correctness_pct     REAL,
    benchmark_stability REAL,
    age_days            INT NOT NULL,
    incident_rate       REAL,
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_reputation_host ON reputation_snapshots (host_id, computed_at DESC);

-- ─── 12. Deployments ─────────────────────────────────────────

CREATE TABLE deployments (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    model_id        UUID NOT NULL REFERENCES models(id) ON DELETE RESTRICT,
    name            TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'scheduling', 'pulling', 'loading', 'warming', 'serving',
        'degraded', 'paused', 'stopping', 'stopped', 'failed'
    )),
    tier            TEXT NOT NULL CHECK (tier IN ('t1', 't2', 't3')),
    region          TEXT NOT NULL DEFAULT 'IN-SOUTH',
    min_replicas    INT NOT NULL DEFAULT 1 CHECK (min_replicas >= 0),
    max_replicas    INT NOT NULL DEFAULT 1 CHECK (max_replicas >= 1),
    scale_to_zero   BOOLEAN NOT NULL DEFAULT FALSE,
    burst_to_spot   BOOLEAN NOT NULL DEFAULT FALSE,
    resident_in     BOOLEAN NOT NULL DEFAULT FALSE,
    endpoint        TEXT,
    quantization    TEXT,
    config_json     JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,
    CONSTRAINT check_replicas CHECK (max_replicas >= min_replicas)
);

CREATE INDEX idx_deployments_org ON deployments (org_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_deployments_state ON deployments (state) WHERE deleted_at IS NULL;

-- Add FK from api_keys to deployments (deferred due to creation order)
ALTER TABLE api_keys ADD CONSTRAINT fk_api_keys_deployment
    FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE SET NULL;

-- ─── 13. Replicas ────────────────────────────────────────────

CREATE TABLE replicas (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    deployment_id   UUID NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    gpu_id          UUID REFERENCES gpus(id),
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'pulling', 'loading', 'warming', 'serving',
        'degraded', 'stopping', 'stopped', 'failed'
    )),
    overlay_ip      INET,
    inference_port  INT,
    image_hash      TEXT,
    model_hash      TEXT,
    started_at      TIMESTAMPTZ,
    stopped_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_replicas_deployment ON replicas (deployment_id) WHERE state = 'serving';
CREATE INDEX idx_replicas_host ON replicas (host_id);

-- ─── 14. Usage Events (APPEND-ONLY, partitioned by month) ───

CREATE TABLE usage_events (
    request_id      UUID NOT NULL,  -- Idempotency key
    deployment_id   UUID NOT NULL,
    replica_id      UUID NOT NULL,
    host_id         UUID NOT NULL,
    input_tokens    INT NOT NULL DEFAULT 0,
    output_tokens   INT NOT NULL DEFAULT 0,
    gpu_seconds     NUMERIC(12,4) NOT NULL DEFAULT 0,
    tier            TEXT NOT NULL,
    amount_customer NUMERIC(12,6) NOT NULL,  -- Amount charged to customer (USD)
    amount_host     NUMERIC(12,6) NOT NULL,  -- Amount accrued to host (USD)
    ts              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status          TEXT NOT NULL DEFAULT 'success' CHECK (status IN ('success', 'error', 'timeout', 'cancelled')),
    PRIMARY KEY (request_id, ts)
) PARTITION BY RANGE (ts);

-- Create initial partitions (current and next month)
-- In production, a cron job creates future partitions.
CREATE TABLE usage_events_2026_08 PARTITION OF usage_events
    FOR VALUES FROM ('2026-08-01') TO ('2026-09-01');
CREATE TABLE usage_events_2026_09 PARTITION OF usage_events
    FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
CREATE TABLE usage_events_2026_10 PARTITION OF usage_events
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

CREATE INDEX idx_usage_deployment_ts ON usage_events (deployment_id, ts);
CREATE INDEX idx_usage_host_ts ON usage_events (host_id, ts);

-- ─── 15. Wallet Ledger ───────────────────────────────────────

CREATE TABLE wallet_ledger (
    entry_id        UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    delta           NUMERIC(14,6) NOT NULL,  -- Positive = credit, negative = debit
    balance_after   NUMERIC(14,6) NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('topup', 'debit', 'credit', 'refund', 'signup_credit')),
    ref_id          UUID,  -- References the source (payment ID, usage aggregation, etc.)
    description     TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_wallet_org ON wallet_ledger (org_id, created_at DESC);

-- ─── 16. Invoices ────────────────────────────────────────────

CREATE TABLE invoices (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE RESTRICT,
    number          TEXT NOT NULL UNIQUE,
    period_start    DATE NOT NULL,
    period_end      DATE NOT NULL,
    subtotal        NUMERIC(14,4) NOT NULL,
    gst_amount      NUMERIC(14,4) NOT NULL DEFAULT 0,
    total           NUMERIC(14,4) NOT NULL,
    status          TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'issued', 'paid', 'void')),
    pdf_url         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_invoices_org ON invoices (org_id, period_start DESC);

-- ─── 17. Invoice Lines ───────────────────────────────────────

CREATE TABLE invoice_lines (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    invoice_id  UUID NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    description TEXT NOT NULL,
    quantity    NUMERIC(14,4) NOT NULL,
    unit_price  NUMERIC(14,6) NOT NULL,
    amount      NUMERIC(14,4) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─── 18. Payout Batches ──────────────────────────────────────

CREATE TABLE payout_batches (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    week_start  DATE NOT NULL,
    week_end    DATE NOT NULL,
    gross       NUMERIC(14,4) NOT NULL,
    tds         NUMERIC(14,4) NOT NULL DEFAULT 0,
    net         NUMERIC(14,4) NOT NULL,
    rzpx_ref    TEXT,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'partial')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─── 19. Payout Lines ────────────────────────────────────────

CREATE TABLE payout_lines (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    batch_id    UUID NOT NULL REFERENCES payout_batches(id) ON DELETE RESTRICT,
    host_id     UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    gross       NUMERIC(14,4) NOT NULL,
    tds_amount  NUMERIC(14,4) NOT NULL DEFAULT 0,
    net         NUMERIC(14,4) NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'failed')),
    rzpx_transfer_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_payout_lines_host ON payout_lines (host_id, created_at DESC);
CREATE INDEX idx_payout_lines_batch ON payout_lines (batch_id);

-- ─── 20. Tax Documents ───────────────────────────────────────

CREATE TABLE tax_docs (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    doc_type        TEXT NOT NULL CHECK (doc_type IN ('form_16a', 'tds_certificate')),
    period_start    DATE NOT NULL,
    period_end      DATE NOT NULL,
    pdf_url         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tax_docs_host ON tax_docs (host_id);

-- ─── 21. Trust Incidents ─────────────────────────────────────

CREATE TABLE trust_incidents (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_id         UUID NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
    kind            TEXT NOT NULL CHECK (kind IN ('mismatch', 'spoof', 'abuse', 'downtime', 'benchmark_drift')),
    evidence_json   JSONB NOT NULL DEFAULT '{}',
    severity        TEXT NOT NULL DEFAULT 'low' CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    action          TEXT CHECK (action IN ('warn', 'demote', 'slash', 'ban')),
    resolved        BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ
);

CREATE INDEX idx_trust_host ON trust_incidents (host_id, created_at DESC);

-- ─── 22. Audit Log ───────────────────────────────────────────

CREATE TABLE audit_log (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    actor_id        UUID NOT NULL,
    actor_type      TEXT NOT NULL CHECK (actor_type IN ('user', 'system', 'admin', 'agent')),
    action          TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     UUID,
    details_json    JSONB,
    ip_address      INET,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_resource ON audit_log (resource_type, resource_id, created_at DESC);
CREATE INDEX idx_audit_actor ON audit_log (actor_id, created_at DESC);

-- ─── 23. Price Book Entries ──────────────────────────────────

CREATE TABLE price_book_entries (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    price_book_id       UUID NOT NULL REFERENCES price_books(id) ON DELETE CASCADE,
    gpu_model           TEXT NOT NULL,
    tier                TEXT NOT NULL CHECK (tier IN ('t1', 't2', 't3')),
    price_per_hour      NUMERIC(12,4) NOT NULL,
    price_in_per_1m     NUMERIC(12,4),
    price_out_per_1m    NUMERIC(12,4),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (price_book_id, gpu_model, tier)
);
