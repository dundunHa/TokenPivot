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
	lockSourceQuery   = `SELECT archived_at FROM sources WHERE id = $1 AND workspace_id = $2 FOR UPDATE`
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

// IssueActive locks the source row, refuses an archived source and inserts the
// credential for the already authorised workspace, all in one READ COMMITTED
// transaction. The first lock is the same one the source archive takes, so
// either the credential commits before the archive revokes it, or the archive
// commits first and this call sees archived_at.
func (p *Postgres) IssueActive(ctx context.Context, workspaceID, sourceID ID, secretHash []byte) (Credential, error) {
	var issued Credential
	err := p.inTx(ctx, "credentials issue", func(tx *pgTx) error {
		var err error
		issued, err = issueActive(ctx, tx, workspaceID, sourceID, secretHash)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return issued, nil
}

// RotateActive reads the old credential, then locks the source row it is fixed
// to (never one supplied by the caller) before inserting the new credential.
func (p *Postgres) RotateActive(ctx context.Context, workspaceID, credentialID ID, secretHash []byte) (Credential, error) {
	var issued Credential
	err := p.inTx(ctx, "credentials rotate", func(tx *pgTx) error {
		var err error
		issued, err = rotateActive(ctx, tx, workspaceID, credentialID, secretHash)
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return issued, nil
}

// inTx runs fn in an explicit READ COMMITTED transaction. A business error from
// fn rolls back and is returned unchanged; a driver failure is made safe.
func (p *Postgres) inTx(ctx context.Context, stage string, fn func(*pgTx) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return database.SafeError(stage, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(&pgTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return database.SafeError(stage, err)
	}
	return nil
}

// pgTx is the transaction-bound surface of issueTx.
type pgTx struct {
	tx pgx.Tx
}

// lockSource locks the source row of the workspace and reports whether it is
// archived. found=false covers missing and foreign sources alike.
func (t *pgTx) lockSource(ctx context.Context, workspaceID, sourceID ID) (found, archived bool, err error) {
	var archivedAt pgtype.Timestamptz
	err = t.tx.QueryRow(ctx, lockSourceQuery, sourceID, workspaceID).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, database.SafeError("credentials source lock", err)
	}
	return true, archivedAt.Valid, nil
}

// byID reads the credential only when it belongs to the workspace.
func (t *pgTx) byID(ctx context.Context, workspaceID, credentialID ID) (Credential, bool, error) {
	credential, err := scanCredential(t.tx.QueryRow(ctx, byIDQuery, credentialID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, false, nil
	}
	if err != nil {
		return Credential{}, false, database.SafeError("credentials lookup", err)
	}
	return credential, true, nil
}

// insert stores one credential row; the id and timestamps use column defaults.
func (t *pgTx) insert(ctx context.Context, workspaceID, sourceID ID, secretHash []byte) (Credential, error) {
	credential, err := scanCredential(t.tx.QueryRow(ctx, insertQuery, workspaceID, sourceID, secretHash))
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
