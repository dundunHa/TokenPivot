// Package projects manages the projects of a workspace. Every operation is
// scoped by workspace id, so a project of another workspace is indistinguishable
// from a missing one. Projects are archived, never deleted: the row, its key and
// every event attributed to it stay in place.
package projects

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ID is the PostgreSQL uuid identity shared by workspaces and projects.
type ID = pgtype.UUID

var (
	// ErrNotFound reports a project that does not belong to the given workspace.
	// It never distinguishes missing rows from foreign ones.
	ErrNotFound = errors.New("projects: not found")
	// ErrConflict reports a project key already used in the workspace, archived
	// rows included.
	ErrConflict = errors.New("projects: conflict")
)

// Project is one stored project. ArchivedAt nil means active.
type Project struct {
	ID          ID
	WorkspaceID ID
	Key         string
	Name        string
	CreatedAt   time.Time
	ArchivedAt  *time.Time
}

// store is the persistence surface. Every method takes the workspace id and
// scopes its SQL by it; none of them decides authorisation.
type store interface {
	List(ctx context.Context, workspaceID ID) ([]Project, error)
	Create(ctx context.Context, workspaceID ID, projectKey, name string) (Project, error)
	Update(ctx context.Context, workspaceID, projectID ID, name string) (Project, error)
	Archive(ctx context.Context, workspaceID, projectID ID) error
}

// Service implements the project use cases on top of a store.
type Service struct {
	store store
}

// New returns a Service backed by the given store.
func New(s store) *Service {
	return &Service{store: s}
}

// List returns every project of the workspace, archived ones included, ordered
// by creation time and id. An empty workspace yields an empty non-nil slice.
func (s *Service) List(ctx context.Context, workspaceID ID) ([]Project, error) {
	return s.store.List(ctx, workspaceID)
}

// Create stores a project in the workspace. A repeated key is ErrConflict.
func (s *Service) Create(ctx context.Context, workspaceID ID, projectKey, name string) (Project, error) {
	return s.store.Create(ctx, workspaceID, projectKey, name)
}

// Update changes the name of a project of the workspace.
func (s *Service) Update(ctx context.Context, workspaceID, projectID ID, name string) (Project, error) {
	return s.store.Update(ctx, workspaceID, projectID, name)
}

// Archive marks a project archived and keeps the first archive time. Archiving
// again succeeds.
func (s *Service) Archive(ctx context.Context, workspaceID, projectID ID) error {
	return s.store.Archive(ctx, workspaceID, projectID)
}
