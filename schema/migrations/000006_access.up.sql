-- Who may join, and who runs the installation.
--
-- Until now anyone who could reach the address could create an account, and
-- operator rights came from a list of emails in the environment. A private
-- network needs neither: accounts are created from invitations, and the first
-- account, proven with a one-time owner code, is the operator.

-- ─── 1. Operators and disabled accounts ──────────────────────

ALTER TABLE users
    ADD COLUMN is_operator BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN disabled_at TIMESTAMPTZ;

COMMENT ON COLUMN users.is_operator IS 'Runs the installation: invites people, sees every machine, can disable accounts.';
COMMENT ON COLUMN users.disabled_at IS 'Set by an operator. A disabled account cannot sign in or refresh a session.';

-- ─── 2. The installation and its owner ───────────────────────
-- One row at most. Claiming it is how the first operator is created, and the
-- primary key is what makes two simultaneous claims yield one owner.

CREATE TABLE installation (
    singleton        BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    owner_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    owner_claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─── 3. Invitations ──────────────────────────────────────────

CREATE TABLE invites (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token_hash  TEXT NOT NULL UNIQUE,  -- SHA-256; the plaintext exists only in the link
    -- NULL: the invited person gets a workspace of their own.
    org_id      UUID REFERENCES organizations(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    email       TEXT,                  -- when set, only this address may use the link
    note        TEXT,                  -- who it is for, as the operator wrote it
    created_by  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    used_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_invites_created ON invites (created_at DESC);
