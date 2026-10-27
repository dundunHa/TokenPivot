package credentials

import (
	"context"
	"errors"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	listQuery         = `SELECT id, workspace_id, source_id, revoked_at FROM ingest_credentials WHERE workspace_id = $1 AND source_id = $2 ORDER BY created_at, id`
	bySecretHashQuery = `SELECT id, workspace_id, source_id, revoked_at FROM ingest_credentials WHERE secret_hash = $1`
	byIDQuery         = `SELECT id, workspace_id, source_id, revoked_at FROM ingest_credentials WHERE id = $1 AND workspace_id = $2`
	sourceExistsQuery = `SELECT 1 FROM sources WHERE id = $1 AND workspace_id = $2`
	insertQuery       = `INSERT INTO ingest_credentials (workspace_id, source_id, secret_hash) VALUES ($1, $2, $3) RETURNING id, workspace_id, source_id, revoked_at`
	revokeQuery       = `UPDATE ingest_credentials SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1 AND workspace_id = $2`
)

// Postgres stores credentials in the ingest_credentials table. Errors leaving
// this type carry fixed text only; SQL, driver messages, tokens and digests are
// never part of an error string.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a store on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// List returns credential metadata for one workspace and source, revoked rows
// included, ordered by creation time.
func (p *Postgres) List(ctx context.Context, workspaceID, sourceID ID) ([]Credential, error) {
	rows, err := p.pool.Query(ctx, listQuery, workspaceID, sourceID)
	if err != nil {
		return nil, database.SafeError("credentials list", err)
	}
	defer rows.Close()
	credentials := []Credential{}
	for rows.Next() {
		credential, err := scanCredential(rows)
		if err != nil {
			return nil, database.SafeError("credentials list", err)
		}
		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeError("credentials list", err)
	}
	return credentials, nil
}

// BySecretHash resolves a credential by its digest. The unique index on
// secret_hash serves this lookup.
func (p *Postgres) BySecretHash(ctx context.Context, secretHash []byte) (Credential, bool, error) {
	credential, err := scanCredential(p.pool.QueryRow(ctx, bySecretHashQuery, secretHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, false, nil
	}
	if err != nil {
		return Credential{}, false, database.SafeError("credentials authenticate", err)
	}
	return credential, true, nil
}

// ByID returns the credential only when it belongs to the workspace.
func (p *Postgres) ByID(ctx context.Context, workspaceID, credentialID ID) (Credential, bool, error) {
	credential, err := scanCredential(p.pool.QueryRow(ctx, byIDQuery, credentialID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, false, nil
	}
	if err != nil {
		return Credential{}, false, database.SafeError("credentials lookup", err)
	}
	return credential, true, nil
}

// SourceExists reports whether the source belongs to the workspace.
func (p *Postgres) SourceExists(ctx context.Context, workspaceID, sourceID ID) (bool, error) {
	var found int
	err := p.pool.QueryRow(ctx, sourceExistsQuery, sourceID, workspaceID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, database.SafeError("credentials source", err)
	}
	return true, nil
}

// Insert stores one credential row; the id and timestamps use column defaults.
func (p *Postgres) Insert(ctx context.Context, workspaceID, sourceID ID, secretHash []byte) (Credential, error) {
	credential, err := scanCredential(p.pool.QueryRow(ctx, insertQuery, workspaceID, sourceID, secretHash))
	if err != nil {
		return Credential{}, database.SafeError("credentials insert", err)
	}
	return credential, nil
}

// Revoke updates one row matched by id and workspace in a single statement.
// The first revoked_at value is kept, so repeated revocation is stable.
func (p *Postgres) Revoke(ctx context.Context, workspaceID, credentialID ID) (bool, error) {
	tag, err := p.pool.Exec(ctx, revokeQuery, credentialID, workspaceID)
	if err != nil {
		return false, database.SafeError("credentials revoke", err)
	}
	return tag.RowsAffected() > 0, nil
}

// scanCredential reads the shared metadata column set. Errors stay raw here so
// callers can test for pgx.ErrNoRows before wrapping them for the outside.
func scanCredential(row pgx.Row) (Credential, error) {
	var credential Credential
	var revokedAt pgtype.Timestamptz
	if err := row.Scan(&credential.ID, &credential.WorkspaceID, &credential.SourceID, &revokedAt); err != nil {
		return Credential{}, err
	}
	if revokedAt.Valid {
		stamp := revokedAt.Time
		credential.RevokedAt = &stamp
	}
	return credential, nil
}
