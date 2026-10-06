-- Agents below the installation's minimum version stay connected, so that
-- they can update themselves, but are given no work until they have.

ALTER TABLE hosts
    ADD COLUMN agent_outdated BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN hosts.agent_outdated IS 'The agent is older than the minimum version of the published release. Set at registration; such a host is not placed on.';
