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
// unique index, the list joins the membership user index, and the created-by
// count uses the prefix of the (created_by_user_id, name) unique index.
const (
	membershipRoleQuery = `SELECT role FROM workspace_memberships WHERE workspace_id = $1 AND user_id = $2`
	listQuery           = `SELECT w.id, w.name
FROM workspace_memberships m
JOIN workspaces w ON w.id = m.workspace_id
WHERE m.user_id = $1
ORDER BY w.name COLLATE "C" ASC, w.id ASC`
	workspaceQuery       = `SELECT id, name FROM workspaces WHERE id = $1`
	lockUserQuery        = `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`
	countCreatedQuery    = `SELECT count(*) FROM workspaces WHERE created_by_user_id = $1`
	insertWorkspaceQuery = `INSERT INTO workspaces (created_by_user_id, name) VALUES ($1, $2) RETURNING id, name`
	insertOwnerQuery     = `INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`
	// workspaceNameIndex is the only unique index whose conflict is a business
	// answer. Every other 23505 stays an internal database failure.
	workspaceNameIndex = "uk_workspaces_created_by_user_id_name"
)

// querier is the subset shared by a pool and an open transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Postgres is the production store and transaction runner. The pool answers
// reads; InTx hands a transaction-bound store to workspace creation, so the
// users row lock outlives a single statement.
type Postgres struct {
	pool *pgxpool.Pool
	q    querier
}

// NewPostgres returns the adapter on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, q: pool}
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
		return "", false, database.SafeStoreError("workspaces membership role", err)
	}
	return Role(role), true, nil
}

// List returns the workspaces the user belongs to, ordered by name in C
// collation and then by id. An account without memberships yields an empty
// non-nil slice.
func (p *Postgres) List(ctx context.Context, userID ID) ([]Workspace, error) {
	rows, err := p.q.Query(ctx, listQuery, userID)
	if err != nil {
		return nil, database.SafeStoreError("workspaces list", err)
	}
	defer rows.Close()
	list := []Workspace{}
	for rows.Next() {
		var workspace Workspace
		if err := rows.Scan(&workspace.ID, &workspace.Name); err != nil {
			return nil, database.SafeStoreError("workspaces list", err)
		}
		list = append(list, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeStoreError("workspaces list", err)
	}
	return list, nil
}

// Workspace returns one workspace by id. found=false means no such row; it
// authorises nothing, so callers resolve MembershipRole first.
func (p *Postgres) Workspace(ctx context.Context, workspaceID ID) (Workspace, bool, error) {
	var workspace Workspace
	err := p.q.QueryRow(ctx, workspaceQuery, workspaceID).Scan(&workspace.ID, &workspace.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Workspace{}, false, nil
	}
	if err != nil {
		return Workspace{}, false, database.SafeStoreError("workspaces lookup", err)
	}
	return workspace, true, nil
}

// InTx runs fn inside one explicit READ COMMITTED transaction. The store passed
// to fn is bound to that transaction, so the workspace and its owner membership
// commit or roll back as one unit. The isolation level is spelled out because
// the quota check relies on READ COMMITTED: after waiting on the users row lock,
// the count statement takes a fresh snapshot and sees the workspaces the previous
// lock holder committed. A nil function result commits; any error rolls back and
// is returned as the caller supplied it.
func (p *Postgres) InTx(ctx context.Context, fn func(context.Context, createStore) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return database.SafeStoreError("workspaces begin", err)
	}
	defer func() {
		// Rollback after a successful commit reports a closed transaction, which
		// is expected and deliberately ignored.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := fn(ctx, &txStore{q: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return database.SafeStoreError("workspaces commit", err)
	}
	return nil
}

// txStore is the transaction-bound creation store.
type txStore struct {
	q querier
}

// lockUser locks the users row and reports whether it exists. It is unexported
// so the fixed creation order stays the only way to reach it.
func (t *txStore) lockUser(ctx context.Context, userID ID) (bool, error) {
	var one int
	err := t.q.QueryRow(ctx, lockUserQuery, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, database.SafeStoreError("workspaces user lock", err)
	}
	return true, nil
}

// countCreatedBy counts the workspaces the account created, whatever their
// memberships. It never decides whether a name is free.
func (t *txStore) countCreatedBy(ctx context.Context, userID ID) (int, error) {
	var count int
	if err := t.q.QueryRow(ctx, countCreatedQuery, userID).Scan(&count); err != nil {
		return 0, database.SafeStoreError("workspaces count", err)
	}
	return count, nil
}

// insertWorkspace stores one workspace; id and created_at use column defaults.
// Only the account-scoped name unique index becomes ErrNameConflict.
func (t *txStore) insertWorkspace(ctx context.Context, userID ID, name string) (Workspace, error) {
	var workspace Workspace
	if err := t.q.QueryRow(ctx, insertWorkspaceQuery, userID, name).Scan(&workspace.ID, &workspace.Name); err != nil {
		if isNameConflict(err) {
			return Workspace{}, ErrNameConflict
		}
		return Workspace{}, database.SafeStoreError("workspaces insert", err)
	}
	return workspace, nil
}

// insertOwnerMembership stores the owner membership of a new workspace.
func (t *txStore) insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error {
	if _, err := t.q.Exec(ctx, insertOwnerQuery, workspaceID, userID); err != nil {
		return database.SafeStoreError("workspaces membership insert", err)
	}
	return nil
}

// isNameConflict reports only the account-scoped name unique index. Every other
// unique violation, including the membership pair, stays an internal failure so
// it can never be shown to the user as "name already exists".
func isNameConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == workspaceNameIndex
}
