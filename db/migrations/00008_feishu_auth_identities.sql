-- Feishu (Lark) identity bindings for browser accounts (T-067).
--
-- Two changes travel together:
--   1. `users.password_hash` becomes nullable, so an account created purely by
--      the Feishu callback exists without a local password. Password login
--      treats a NULL hash as "wrong password" after paying the same Argon2id
--      cost, so the absence of a local password is not observable.
--   2. `user_identities` records one external identity per provider and user.
--      The first release only writes `provider = 'feishu'`, where
--      `provider_user_id` is the Feishu open_id and `union_id` / `tenant_key`
--      are kept for a later cross-application migration.
--
-- Unlike `00005_auth_users_sessions.sql`, this table declares its reference and
-- range rules in the schema (decided by the T-067 plan): the reference to
-- `users` and the two non-empty CHECK constraints guard the binding rows that
-- the callback and the bind endpoint write. Session and password policy stay in
-- the application.
--
-- The Down section restores `password_hash NOT NULL`, which needs a value for
-- rows created without a local password. '!sso-disabled' is not a valid
-- Argon2id encoding, so those accounts cannot log in with a password after a
-- rollback and must use Feishu again.
-- +goose Up
ALTER TABLE users
    ALTER COLUMN password_hash DROP NOT NULL;

CREATE TABLE user_identities (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider         text        COLLATE "C" NOT NULL,
    provider_user_id text        COLLATE "C" NOT NULL,
    union_id         text        COLLATE "C",
    tenant_key       text        COLLATE "C",
    email            text        COLLATE "C" NOT NULL,
    display_name     text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_user_identities_provider_nonempty CHECK (btrim(provider) <> ''),
    CONSTRAINT chk_user_identities_provider_uid_nonempty CHECK (btrim(provider_user_id) <> ''),
    CONSTRAINT uk_user_identities_provider_uid UNIQUE (provider, provider_user_id),
    CONSTRAINT uk_user_identities_user_provider UNIQUE (user_id, provider)
);

CREATE INDEX idx_user_identities_user_id ON user_identities (user_id);

COMMENT ON TABLE user_identities IS 'External identity bindings for browser accounts. One row links a provider identity to exactly one users row; at most one identity per (user, provider), so a user cannot bind two Feishu open_ids.';
COMMENT ON COLUMN user_identities.provider IS 'Identity provider key, compared bytewise under the C collation. The first release only writes feishu.';
COMMENT ON COLUMN user_identities.provider_user_id IS 'Provider-side stable user identifier; for feishu this is the open_id. Unique together with provider.';
COMMENT ON COLUMN user_identities.union_id IS 'Optional Feishu union_id, kept for a later migration across Feishu applications. Never used for lookups in the first release.';
COMMENT ON COLUMN user_identities.tenant_key IS 'Optional Feishu tenant key of the authorizing organization. Informational only; no tenant isolation is derived from it.';
COMMENT ON COLUMN user_identities.email IS 'Email returned by the provider at bind time, normalized like users.email. Not the account identity: accounts are matched by users.email.';
COMMENT ON COLUMN user_identities.display_name IS 'Optional display name returned by the provider. Informational only; users has no display name column.';
GRANT SELECT, INSERT, UPDATE ON user_identities TO tokenpivot_app;

-- +goose Down
DROP TABLE IF EXISTS user_identities;

UPDATE users SET password_hash = '!sso-disabled' WHERE password_hash IS NULL;
ALTER TABLE users
    ALTER COLUMN password_hash SET NOT NULL;
