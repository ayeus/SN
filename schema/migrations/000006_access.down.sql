DROP TABLE IF EXISTS invites;
DROP TABLE IF EXISTS installation;
ALTER TABLE users
    DROP COLUMN IF EXISTS disabled_at,
    DROP COLUMN IF EXISTS is_operator;
