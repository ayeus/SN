-- AyeusANN Schema v3: Host Telemetry & Heartbeat History
-- Migration: 000003_host_telemetry
--
-- Persists host heartbeats (CPU, RAM, GPU status, active jobs, timestamps)
-- so the Trust Engine and dashboard can compute real uptime, health, and reputation.

CREATE TABLE host_telemetry (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    host_id             UUID NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    cpu_usage_pct       REAL NOT NULL DEFAULT 0.0,
    memory_usage_pct    REAL NOT NULL DEFAULT 0.0,
    gpu_status          JSONB NOT NULL DEFAULT '[]',
    active_jobs         INT NOT NULL DEFAULT 0,
    host_user_active    BOOLEAN NOT NULL DEFAULT FALSE,
    ts                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_host_telemetry_host_ts ON host_telemetry (host_id, ts DESC);
CREATE INDEX idx_host_telemetry_ts ON host_telemetry (ts DESC);

COMMENT ON TABLE host_telemetry IS
    'High-resolution telemetry samples recorded during host agent heartbeats.';
