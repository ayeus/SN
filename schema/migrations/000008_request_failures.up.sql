-- What went wrong on the way to an answer.
--
-- usage_events holds one row for every request a replica answered, well or
-- badly. Two things never reached it: an attempt on one replica that failed
-- before anything was sent back (the gateway quietly tried another), and a
-- request that was turned away without being served at all. A host could fail
-- every request it was given and show a perfect record.

CREATE TABLE request_failures (
    id            BIGSERIAL PRIMARY KEY,
    ts            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    request_id    UUID NOT NULL,
    org_id        UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    deployment_id UUID REFERENCES deployments(id) ON DELETE CASCADE,
    replica_id    UUID,
    host_id       UUID,
    model_id      UUID,
    kind          TEXT NOT NULL CHECK (kind IN ('attempt', 'refused')),
    reason        TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '',
    status_code   INT NOT NULL DEFAULT 0
);

CREATE INDEX idx_request_failures_host_ts ON request_failures (host_id, ts) WHERE host_id IS NOT NULL;
CREATE INDEX idx_request_failures_deployment_ts ON request_failures (deployment_id, ts);
CREATE INDEX idx_request_failures_org_ts ON request_failures (org_id, ts);
CREATE INDEX idx_request_failures_request ON request_failures (request_id);

COMMENT ON TABLE request_failures IS 'Failed attempts on a replica (kind attempt; the request may still have been answered elsewhere) and requests answered with an error without being served (kind refused).';
COMMENT ON COLUMN request_failures.reason IS 'attempt: host_error, host_timeout, host_disconnected (these three count against the host), not_serving, agent_too_old, unreachable. refused: deployment_not_ready, no_healthy_replica, at_capacity, timeout.';

-- Every request a caller made and how it ended, served or not. Dashboards
-- count from this, so a request that was turned away is an error there too.
CREATE VIEW request_outcomes AS
    SELECT ts, org_id, deployment_id, status, input_tokens, output_tokens, duration_ms, TRUE AS served
    FROM usage_events
    UNION ALL
    SELECT ts, org_id, deployment_id, 'error', 0, 0, 0, FALSE
    FROM request_failures
    WHERE kind = 'refused' AND deployment_id IS NOT NULL;
