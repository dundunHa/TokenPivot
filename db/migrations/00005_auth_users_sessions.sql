-- Browser accounts and sessions (T-016). Only an Argon2id encoding of a password
-- and a SHA-256 digest of a session token are stored. Referential integrity,
-- value ranges and session policy are enforced by the application: no foreign
-- keys, CHECK, triggers or RLS.
-- +goose Up
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    email text COLLATE "C" NOT NULL,
    password_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_users_email ON users (email);
CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL,
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_sessions_token_hash ON sessions (token_hash);
CREATE INDEX idx_sessions_user_id ON sessions (user_id);
COMMENT ON TABLE users IS 'Browser accounts. The email is stored only in normalized form; the password column holds an Argon2id encoding, never plaintext or a fast digest.';
COMMENT ON COLUMN users.email IS 'Normalized account identity: trimmed, lowercased and unique under the C collation, so equality is bytewise and independent of the database locale.';
COMMENT ON COLUMN users.password_hash IS 'Standard Argon2id encoded string including the algorithm, version, parameters, salt and derived key.';
COMMENT ON TABLE sessions IS 'Browser sessions backing the tokenpivot_session cookie. Each row holds one opaque token digest; rows are expired or revoked, never deleted by the service role.';
COMMENT ON COLUMN sessions.token_hash IS 'Raw 32-byte SHA-256 digest of the full tps_ token. The plaintext token is never stored.';
COMMENT ON COLUMN sessions.expires_at IS 'Sliding 30-day expiry, written by the issuing statement and renewed by every authenticated request.';
COMMENT ON COLUMN sessions.revoked_at IS 'NULL means active; otherwise the time the session was revoked by logout.';
GRANT SELECT, INSERT, UPDATE ON users, sessions TO tokenpivot_app;

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
