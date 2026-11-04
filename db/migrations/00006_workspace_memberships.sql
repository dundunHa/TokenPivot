-- +goose Up
CREATE TABLE workspace_memberships (
    workspace_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uk_workspace_memberships_workspace_id_user_id
    ON workspace_memberships (workspace_id, user_id);
CREATE INDEX idx_workspace_memberships_user_id
    ON workspace_memberships (user_id);
COMMENT ON TABLE workspace_memberships IS
    'User membership and role within a workspace. Creating a workspace writes its owner membership in the same transaction.';
COMMENT ON COLUMN workspace_memberships.role IS
    'Application-validated role: owner or member. Creation writes owner; member is read-only.';
GRANT SELECT, INSERT ON workspace_memberships TO tokenpivot_app;

-- +goose Down
DROP TABLE workspace_memberships;
