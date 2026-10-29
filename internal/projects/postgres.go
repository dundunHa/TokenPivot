package projects

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
	columns       = `id, workspace_id, project_key, name, created_at, archived_at`
	listQuery     = `SELECT ` + columns + ` FROM projects WHERE workspace_id = $1 ORDER BY created_at, id`
	createQuery   = `INSERT INTO projects (workspace_id, project_key, name) VALUES ($1, $2, $3) RETURNING ` + columns
	updateQuery   = `UPDATE projects SET name = $3 WHERE id = $1 AND workspace_id = $2 RETURNING ` + columns
	archiveQuery  = `UPDATE projects SET archived_at = COALESCE(archived_at, now()) WHERE id = $1 AND workspace_id = $2`
	uniqueKeyName = "uk_projects_workspace_id_project_key"
)

// Postgres stores projects in the projects table. Errors leaving this type carry
// fixed text only.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns a store on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

// List returns the workspace projects, archived ones included.
func (p *Postgres) List(ctx context.Context, workspaceID ID) ([]Project, error) {
	rows, err := p.pool.Query(ctx, listQuery, workspaceID)
	if err != nil {
		return nil, database.SafeError("projects list", err)
	}
	defer rows.Close()
	list := []Project{}
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, database.SafeError("projects list", err)
		}
		list = append(list, project)
	}
	if err := rows.Err(); err != nil {
		return nil, database.SafeError("projects list", err)
	}
	return list, nil
}

// Create inserts a project into the path workspace. Only the project key unique
// index is a business conflict; any other failure stays a safe database error.
func (p *Postgres) Create(ctx context.Context, workspaceID ID, projectKey, name string) (Project, error) {
	project, err := scanProject(p.pool.QueryRow(ctx, createQuery, workspaceID, projectKey, name))
	if err != nil {
		if isUniqueViolation(err) {
			return Project{}, ErrConflict
		}
		return Project{}, database.SafeError("projects create", err)
	}
	return project, nil
}

// Update renames a project matched by id and workspace in one statement.
func (p *Postgres) Update(ctx context.Context, workspaceID, projectID ID, name string) (Project, error) {
	project, err := scanProject(p.pool.QueryRow(ctx, updateQuery, projectID, workspaceID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, database.SafeError("projects update", err)
	}
	return project, nil
}

// Archive stamps archived_at once; a repeated call matches the row and keeps
// the first value.
func (p *Postgres) Archive(ctx context.Context, workspaceID, projectID ID) error {
	tag, err := p.pool.Exec(ctx, archiveQuery, projectID, workspaceID)
	if err != nil {
		return database.SafeError("projects archive", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == uniqueKeyName
}

func scanProject(row pgx.Row) (Project, error) {
	var project Project
	var createdAt, archivedAt pgtype.Timestamptz
	if err := row.Scan(&project.ID, &project.WorkspaceID, &project.Key, &project.Name, &createdAt, &archivedAt); err != nil {
		return Project{}, err
	}
	project.CreatedAt = createdAt.Time
	if archivedAt.Valid {
		stamp := archivedAt.Time
		project.ArchivedAt = &stamp
	}
	return project, nil
}
