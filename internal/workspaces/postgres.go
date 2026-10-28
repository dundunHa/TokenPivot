package workspaces

import (
	"context"
	"errors"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQL statements are spelled out once. The membership read uses the composite
// unique index, and the owner lookup uses idx_workspace_memberships_user_id.
const (
	membershipRoleQuery    = `SELECT role FROM workspace_memberships WHERE workspace_id = $1 AND user_id = $2`
	lockUserQuery          = `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`
	personalWorkspaceQuery = `SELECT w.id, w.name
FROM workspace_memberships m
JOIN workspaces w ON w.id = m.workspace_id
WHERE m.user_id = $1 AND m.role = 'owner'
ORDER BY m.joined_at, w.id
LIMIT 1`
	insertWorkspaceQuery       = `INSERT INTO workspaces (name) VALUES ($1) RETURNING id, name`
	insertOwnerMembershipQuery = `INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`
)

// querier is the subset shared by a pool and an open transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Postgres is the pool-bound adapter. It deliberately exposes only the boundary
// read: personal workspace creation needs the users row lock to outlive a single
// statement, so it is not reachable from a pooled connection.
type Postgres struct {
	q querier
}

// NewPostgres returns the pool-bound adapter on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{q: pool}
}

// MembershipRole returns the stored role of one user in one workspace. An
// unknown pair is found=false; every database failure is a real error, so a
// broken database is never reported as a non-member.
func (p *Postgres) MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error) {
	var role string
	err := p.q.QueryRow(ctx, membershipRoleQuery, workspaceID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, database.SafeError("workspaces membership role", err)
	}
	return Role(role), true, nil
}

// Tx is the transaction-bound adapter. It can only be built from an open pgx.Tx,
// which keeps CreatePersonal inside the caller's transaction and keeps the users
// row lock held until that transaction ends. It never begins, commits or rolls
// back.
type Tx struct {
	q querier
}

// NewTx returns the creation adapter bound to the caller's transaction.
func NewTx(tx pgx.Tx) *Tx {
	return &Tx{q: tx}
}

// CreatePersonal returns the initial personal workspace of the user, creating it
// and its owner membership the first time. The order is fixed: lock the users
// row, look up an existing owner workspace, insert only if absent. Under READ
// COMMITTED a second transaction waits on the row lock and then sees the first
// committed membership, so repeat and concurrent calls converge on one
// workspace. Any error means the caller must roll back the whole transaction.
func (t *Tx) CreatePersonal(ctx context.Context, userID ID) (Workspace, error) {
	return createPersonal(ctx, t, userID)
}

// lockUser locks the users row and reports whether it exists. It is unexported
// so the fixed creation order stays the only way to reach it.
func (t *Tx) lockUser(ctx context.Context, userID ID) (bool, error) {
	var one int
	err := t.q.QueryRow(ctx, lockUserQuery, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, database.SafeError("workspaces user lock", err)
	}
	return true, nil
}

// personalWorkspace returns the workspace the user already owns, if any. This
// only answers "current workspace" while every workspace is personal; it must be
// re-examined before multiple workspaces per user exist.
func (t *Tx) personalWorkspace(ctx context.Context, userID ID) (Workspace, bool, error) {
	var workspace Workspace
	err := t.q.QueryRow(ctx, personalWorkspaceQuery, userID).Scan(&workspace.ID, &workspace.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, database.SafeError("workspaces personal lookup", err)
	}
	return workspace, true, nil
}

// insertWorkspace stores one workspace; id and created_at use column defaults.
func (t *Tx) insertWorkspace(ctx context.Context, name string) (Workspace, error) {
	var workspace Workspace
	if err := t.q.QueryRow(ctx, insertWorkspaceQuery, name).Scan(&workspace.ID, &workspace.Name); err != nil {
		return Workspace{}, database.SafeError("workspaces insert", err)
	}
	return workspace, nil
}

// insertOwnerMembership stores the owner membership of a personal workspace.
func (t *Tx) insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error {
	if _, err := t.q.Exec(ctx, insertOwnerMembershipQuery, workspaceID, userID); err != nil {
		return database.SafeError("workspaces membership insert", err)
	}
	return nil
}
