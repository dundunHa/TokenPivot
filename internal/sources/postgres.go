package sources

import (
	"context"
	"errors"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	columns         = `id, workspace_id, platform, instance_key, created_at, last_received_at, archived_at`
	listQuery       = `SELECT ` + columns + ` FROM sources WHERE workspace_id = $1 ORDER BY created_at, id`
	createQuery     = `INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1, $2, $3) RETURNING ` + columns
	updateQuery     = `UPDATE sources SET platform = COALESCE($3, platform), instance_key = COALESCE($4, instance_key) WHERE id = $1 AND workspace_id = $2 RETURNING ` + columns
	lockQuery       = `SELECT 1 FROM sources WHERE id = $1 AND workspace_id = $2 FOR UPDATE`
	archiveQuery    = `UPDATE sources SET archived_at = COALESCE(archived_at, now()) WHERE id = $1 AND workspace_id = $2`
	revokeQuery     = `UPDATE ingest_credentials SET revoked_at = now() WHERE workspace_id = $1 AND source_id = $2 AND revoked_at IS NULL`
	uniqueIdentName = "uk_sources_workspace_id_platform_instance_key"
)

// Postgres stores sources in the sources table. Errors leaving this type carry
// fixed text only.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a store on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// List returns the workspace sources, archived ones included.
func (p *Postgres) List(ctx context.Context, workspaceID ID) ([]Source, error) {
	rows, err := p.pool.Query(ctx, listQuery, workspaceID)
	if err != nil {
		return nil, database.SafeError("sources list", err)
	}
	defer rows.Close()
	list := []Source{}
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, database.SafeError("sources list", err)
		}
		list = append(list, source)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeError("sources list", err)
	}
	return list, nil
}

// Create inserts a source into the path workspace. Only the platform/instance
// unique index is a business conflict.
func (p *Postgres) Create(ctx context.Context, workspaceID ID, platform, instanceKey string) (Source, error) {
	source, err := scanSource(p.pool.QueryRow(ctx, createQuery, workspaceID, platform, instanceKey))
	if err != nil {
		if isUniqueViolation(err) {
			return Source{}, ErrConflict
		}
		return Source{}, database.SafeError("sources create", err)
	}
	return source, nil
}

// Update changes platform and/or instance_key of a source matched by id and
// workspace. The statement is fixed; nil pointers bind as NULL and keep the
// stored value, so no caller text becomes SQL.
func (p *Postgres) Update(ctx context.Context, workspaceID, sourceID ID, platform, instanceKey *string) (Source, error) {
	source, err := scanSource(p.pool.QueryRow(ctx, updateQuery, sourceID, workspaceID, platform, instanceKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return Source{}, ErrConflict
		}
		return Source{}, database.SafeError("sources update", err)
	}
	return source, nil
}

// Archive archives the source and revokes its active credentials in one READ
// COMMITTED transaction. Only a successful Commit reports success.
func (p *Postgres) Archive(ctx context.Context, workspaceID, sourceID ID) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return database.SafeError("sources archive", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := archive(ctx, &pgTx{tx: tx}, workspaceID, sourceID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return database.SafeError("sources archive commit", err)
	}
	return nil
}

// pgTx binds the archive steps to one open pgx transaction.
type pgTx struct {
	tx pgx.Tx
}

func (t *pgTx) lockSource(ctx context.Context, workspaceID, sourceID ID) (bool, error) {
	var one int
	err := t.tx.QueryRow(ctx, lockQuery, sourceID, workspaceID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, database.SafeError("sources lock", err)
	}
	return true, nil
}

func (t *pgTx) markArchived(ctx context.Context, workspaceID, sourceID ID) error {
	if _, err := t.tx.Exec(ctx, archiveQuery, sourceID, workspaceID); err != nil {
		return database.SafeError("sources archive", err)
	}
	return nil
}

func (t *pgTx) revokeCredentials(ctx context.Context, workspaceID, sourceID ID) error {
	if _, err := t.tx.Exec(ctx, revokeQuery, workspaceID, sourceID); err != nil {
		return database.SafeError("sources revoke credentials", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == uniqueIdentName
}

func scanSource(row pgx.Row) (Source, error) {
	var source Source
	var createdAt, lastReceivedAt, archivedAt pgtype.Timestamptz
	if err := row.Scan(&source.ID, &source.WorkspaceID, &source.Platform, &source.InstanceKey, &createdAt, &lastReceivedAt, &archivedAt); err != nil {
		return Source{}, err
	}
	source.CreatedAt = createdAt.Time
	if lastReceivedAt.Valid {
		stamp := lastReceivedAt.Time
		source.LastReceivedAt = &stamp
	}
	if archivedAt.Valid {
		stamp := archivedAt.Time
		source.ArchivedAt = &stamp
	}
	return source, nil
}
