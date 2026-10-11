-- A host that is giving its GPU back to its owner for now.
--
-- Different from paused, which the owner sets in the console and which stays
-- until they clear it: yielding is decided on the machine, from rules the
-- owner chose there (on battery, the GPU busy with something else, a pause
-- from the command line, hours of the day), and clears by itself.

ALTER TABLE hosts
    ADD COLUMN yielding       BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN yield_reason   TEXT,
    ADD COLUMN yielding_since TIMESTAMPTZ;

COMMENT ON COLUMN hosts.yielding IS 'The agent reports that the machine''s owner needs it. No work is placed on it and what it was running is moved.';
