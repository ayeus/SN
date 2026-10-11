-- Placement that learns, hosts that tell the truth.

-- Start failures are counted from here. Set when a deployment is created and
-- again when it is resumed or retried, so an old run of failures is not held
-- against a new attempt.
ALTER TABLE deployments
    ADD COLUMN placing_since TIMESTAMPTZ NOT NULL DEFAULT NOW();

COMMENT ON COLUMN deployments.placing_since IS 'Start failures before this moment do not count towards giving the deployment up.';

-- What a host last got wrong and how far its clock is from the platform's,
-- so its owner can see both without reading logs.
ALTER TABLE hosts
    ADD COLUMN last_error         TEXT,
    ADD COLUMN last_error_at      TIMESTAMPTZ,
    ADD COLUMN clock_skew_seconds INT;

COMMENT ON COLUMN hosts.clock_skew_seconds IS 'Host clock minus platform clock at the last heartbeat, in seconds. Signed jobs expire, so a clock far out makes a host refuse them.';

-- The scheduler asks, every pass, which machines recently failed to start
-- which model.
CREATE INDEX idx_replicas_start_failures ON replicas (host_id, stopped_at)
    WHERE state = 'failed' AND started_at IS NULL;
