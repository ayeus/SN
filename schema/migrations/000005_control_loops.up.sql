-- AyeusANN Schema v5: control loops, inference tunnel, honest lifecycle
-- Migration: 000005_control_loops
--
-- Before this migration a deployment was inserted, a replica row was written,
-- and nothing else ever happened: no component dispatched work to a host, no
-- component moved a deployment to 'serving', and the router could therefore
-- never route a request. This migration adds the state the reconciliation
-- loops need (ADR-011):
--
--   scheduler   : deployments.desired_state  → places / stops replicas
--   coordinator : replicas.dispatched_at     → sends manifests to agents
--   agent       : stage events               → replica state
--   everyone    : deployment_events          → auditable transition log (FR-21)

-- ─── 1. Catalogue: how each runtime names a model ────────────
-- runtime_refs maps a runtime ("ollama", "vllm") to the model identifier inside
-- that runtime and the memory it needs there. Ollama serves 4-bit builds, vLLM
-- serves full precision, so the VRAM floor genuinely differs per runtime.
--   {"ollama": {"model": "qwen2.5:7b", "quantization": "q4_K_M", "min_vram_gb": 6}}
ALTER TABLE models
    ADD COLUMN runtime_refs   JSONB NOT NULL DEFAULT '{}',
    ADD COLUMN description    TEXT,
    ADD COLUMN context_length INT;

-- ─── 2. Hosts: durable identity, runtime, cache, owner org ───
-- Registration tokens are single-use, so before this a host that restarted or
-- lost its connection could never come back. A host credential (hashed here,
-- held by the agent) re-authenticates every reconnect.
ALTER TABLE hosts
    ADD COLUMN credential_hash TEXT,
    ADD COLUMN org_id          UUID REFERENCES organizations (id) ON DELETE SET NULL,
    ADD COLUMN os              TEXT,
    ADD COLUMN runtime         TEXT,
    ADD COLUMN runtime_healthy BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN cached_models   TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN paused          BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN last_seen_ip    TEXT;

CREATE UNIQUE INDEX idx_hosts_credential ON hosts (credential_hash) WHERE credential_hash IS NOT NULL;
CREATE INDEX idx_hosts_user ON hosts (user_id) WHERE deleted_at IS NULL;

-- ─── 3. Replicas: dispatch bookkeeping and honest errors ─────
ALTER TABLE replicas
    ADD COLUMN dispatched_at TIMESTAMPTZ,
    ADD COLUMN stop_sent_at  TIMESTAMPTZ,
    ADD COLUMN detail        TEXT,
    ADD COLUMN last_error    TEXT,
    ADD COLUMN updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX idx_replicas_state ON replicas (state);

-- One replica per GPU (ADR-008). The GPU row records who holds it so a stopped
-- or failed replica can release exactly the device it reserved.
ALTER TABLE gpus
    ADD COLUMN replica_id UUID REFERENCES replicas (id) ON DELETE SET NULL;

-- ─── 4. Deployments: desired vs observed state ───────────────
-- state is what the platform observes; desired_state is what the customer asked
-- for. The scheduler converges the first toward the second.
ALTER TABLE deployments
    ADD COLUMN desired_state TEXT NOT NULL DEFAULT 'running'
        CHECK (desired_state IN ('running', 'paused', 'stopped'));

-- ─── 5. Deployment event log (FR-21: all transitions logged) ─
CREATE TABLE deployment_events (
    id            BIGSERIAL PRIMARY KEY,
    deployment_id UUID NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    replica_id    UUID,
    kind          TEXT NOT NULL CHECK (kind IN ('state', 'replica', 'info', 'error')),
    state         TEXT,
    message       TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_deployment_events_dep ON deployment_events (deployment_id, id DESC);

-- ─── 6. Usage events: attribution and currency ───────────────
-- Adding columns to the partitioned parent propagates to every partition.
-- currency records the wallet currency the amounts are denominated in; Indian
-- organisations are billed in INR (PRD §1 decision 2, SRS FR-70).
ALTER TABLE usage_events
    ADD COLUMN org_id      UUID,
    ADD COLUMN model_id    UUID,
    ADD COLUMN duration_ms INT,
    ADD COLUMN currency    CHAR(3) NOT NULL DEFAULT 'USD',
    ADD COLUMN fx_rate     NUMERIC(18, 8) NOT NULL DEFAULT 1;

CREATE INDEX idx_usage_org_ts ON usage_events (org_id, ts);

-- ─── 7. Invoices: the column holds any tax, not only GST ─────
-- billing-meter wrote tax_amount while the column was still gst_amount, so every
-- invoice insert failed.
ALTER TABLE invoices RENAME COLUMN gst_amount TO tax_amount;

-- ─── 8. FX rates ─────────────────────────────────────────────
-- Catalogue token prices are USD per 1M tokens (PRD §9); wallets are held in the
-- organisation's currency (INR for India). Usage is converted at the recorded
-- rate, and the rate is stored on each usage event so history reproduces.
CREATE TABLE fx_rates (
    base       CHAR(3) NOT NULL,
    quote      CHAR(3) NOT NULL,
    rate       NUMERIC(18, 8) NOT NULL CHECK (rate > 0),
    source     TEXT NOT NULL DEFAULT 'manual',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (base, quote)
);

INSERT INTO fx_rates (base, quote, rate, source) VALUES
    ('USD', 'USD', 1.0,   'identity'),
    ('USD', 'INR', 84.00, 'manual seed 2026-09 (PRD rate card implies ~84); replace with a daily feed'),
    ('USD', 'EUR', 0.92,  'manual seed 2026-09; replace with a daily feed'),
    ('USD', 'GBP', 0.78,  'manual seed 2026-09; replace with a daily feed');

-- ─── 9. Public GPU rate card (PRD §9) ────────────────────────
-- The deploy wizard shows GPU cards with ₹/hr and $/hr and live availability
-- (PRD F-3), and the host earnings calculator needs power draw (PRD F-13).
-- Both read this table instead of numbers baked into the UI.
CREATE TABLE gpu_skus (
    id                 TEXT PRIMARY KEY,           -- e.g. 'rtx-4090-t2'
    gpu_model          TEXT NOT NULL,              -- display name
    match_pattern      TEXT NOT NULL,              -- ILIKE pattern matched against gpus.model
    vram_gb            INT NOT NULL,
    tdp_watts          INT NOT NULL,
    tier               TEXT NOT NULL CHECK (tier IN ('t1', 't2', 't3')),
    price_per_hour_inr NUMERIC(12, 2) NOT NULL,
    price_per_hour_usd NUMERIC(12, 4) NOT NULL,
    is_spot            BOOLEAN NOT NULL DEFAULT FALSE,
    display_order      INT NOT NULL DEFAULT 100,
    active             BOOLEAN NOT NULL DEFAULT TRUE
);

-- ─── 10. Idempotency keys (IR-1: honoured on all POSTs) ──────
CREATE TABLE idempotency_keys (
    scope         TEXT NOT NULL,        -- org id, or 'anon' for unauthenticated POSTs
    key           TEXT NOT NULL,
    method        TEXT NOT NULL,
    path          TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    status_code   INT,
    response_body BYTEA,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (scope, key)
);

CREATE INDEX idx_idempotency_created ON idempotency_keys (created_at);
