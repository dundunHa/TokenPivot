-- Foundation tables (T-008). Referential integrity and same-workspace consistency
-- are enforced by the application: no foreign keys, CHECK, triggers or RLS.
-- +goose Up
CREATE TABLE workspaces (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE projects (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    project_key text COLLATE "C" NOT NULL,
    name text NOT NULL,
    archived_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_projects_workspace_id_project_key ON projects (workspace_id, project_key);
CREATE TABLE sources (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    platform text COLLATE "C" NOT NULL,
    instance_key text COLLATE "C" NOT NULL,
    last_received_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_sources_workspace_id_platform_instance_key ON sources (workspace_id, platform, instance_key);
CREATE TABLE ingest_credentials (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    workspace_id uuid NOT NULL,
    source_id uuid NOT NULL,
    secret_hash bytea NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_ingest_credentials_secret_hash ON ingest_credentials (secret_hash);
CREATE INDEX idx_ingest_credentials_workspace_id_source_id ON ingest_credentials (workspace_id, source_id);
COMMENT ON TABLE workspaces IS 'Workspace tenancy root. Every other row is scoped to exactly one workspace.';
COMMENT ON TABLE projects IS 'Projects own usage attribution inside a workspace. Archiving keeps the project key reserved.';
COMMENT ON TABLE sources IS 'Stable plugin instance identity per workspace. One row spans credential rotations and revocations.';
COMMENT ON TABLE ingest_credentials IS 'Ingest credentials stored only as a SHA-256 digest. Revoked rows are kept for rotation history.';
COMMENT ON COLUMN projects.archived_at IS 'NULL means active; otherwise the time the project was archived.';
COMMENT ON COLUMN sources.platform IS 'Machine identifier of the plugin platform, compared bytewise under the C collation.';
COMMENT ON COLUMN sources.instance_key IS 'Machine identifier of one plugin instance, compared bytewise under the C collation.';
COMMENT ON COLUMN sources.last_received_at IS 'NULL means no data was ever received; otherwise the time of the last successful receive.';
COMMENT ON COLUMN ingest_credentials.secret_hash IS 'Raw 32-byte SHA-256 digest of the full token. The plaintext token is never stored.';
COMMENT ON COLUMN ingest_credentials.revoked_at IS 'NULL means active; otherwise the time the credential was revoked by rotation or by an owner.';
GRANT SELECT, INSERT, UPDATE ON workspaces, projects, sources, ingest_credentials TO tokenpivot_app;

-- +goose Down
DROP TABLE ingest_credentials;
DROP TABLE sources;
DROP TABLE projects;
DROP TABLE workspaces;
