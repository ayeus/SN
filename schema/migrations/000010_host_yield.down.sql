ALTER TABLE hosts
    DROP COLUMN IF EXISTS yielding_since,
    DROP COLUMN IF EXISTS yield_reason,
    DROP COLUMN IF EXISTS yielding;
