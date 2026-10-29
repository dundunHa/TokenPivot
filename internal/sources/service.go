// Package sources manages the data sources (plugin instances) of a workspace.
// Every operation is scoped by workspace id. Sources are archived, never
// deleted: archiving keeps the row, its events and its credential rows, and
// revokes the credentials that are still active in the same transaction.
package sources

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ID is the PostgreSQL uuid identity shared by workspaces and sources.
type ID = pgtype.UUID

var (
	// ErrNotFound reports a source that does not belong to the given workspace.
	// It never distinguishes missing rows from foreign ones.
	ErrNotFound = errors.New("sources: not found")
	// ErrConflict reports a (platform, instance_key) already used in the
	// workspace, archived rows included.
	ErrConflict = errors.New("sources: conflict")
)

// Source is one stored data source. ArchivedAt nil means active.
type Source struct {
	ID             ID
	WorkspaceID    ID
	Platform       string
	InstanceKey    string
	CreatedAt      time.Time
	LastReceivedAt *time.Time
	ArchivedAt     *time.Time
}

// store is the persistence surface. Every method takes the workspace id and
// scopes its SQL by it; none of them decides authorisation.
type store interface {
	List(ctx context.Context, workspaceID ID) ([]Source, error)
	Create(ctx context.Context, workspaceID ID, platform, instanceKey string) (Source, error)
	Update(ctx context.Context, workspaceID, sourceID ID, platform, instanceKey *string) (Source, error)
	Archive(ctx context.Context, workspaceID, sourceID ID) error
}

// Service implements the source use cases on top of a store.
type Service struct {
	store store
}

// New returns a Service backed by the given store.
func New(s store) *Service {
	return &Service{store: s}
}

// List returns every source of the workspace, archived ones included.
func (s *Service) List(ctx context.Context, workspaceID ID) ([]Source, error) {
	return s.store.List(ctx, workspaceID)
}

// Create stores a source in the workspace. It creates no credential.
func (s *Service) Create(ctx context.Context, workspaceID ID, platform, instanceKey string) (Source, error) {
	return s.store.Create(ctx, workspaceID, platform, instanceKey)
}

// Update changes the machine identity of a source; nil fields stay unchanged.
// Callers must pass at least one field.
func (s *Service) Update(ctx context.Context, workspaceID, sourceID ID, platform, instanceKey *string) (Source, error) {
	return s.store.Update(ctx, workspaceID, sourceID, platform, instanceKey)
}

// Archive archives the source and revokes its active credentials atomically.
// Archiving again succeeds and changes no stamp.
func (s *Service) Archive(ctx context.Context, workspaceID, sourceID ID) error {
	return s.store.Archive(ctx, workspaceID, sourceID)
}

// archiveTx is the transaction-bound surface of archiving. Only a store bound
// to an open transaction satisfies it, so the source row lock is held until
// that transaction ends.
type archiveTx interface {
	// lockSource locks the source row of the workspace (SELECT ... FOR UPDATE)
	// and reports whether it exists.
	lockSource(ctx context.Context, workspaceID, sourceID ID) (bool, error)
	// markArchived stamps archived_at once, keeping the first value.
	markArchived(ctx context.Context, workspaceID, sourceID ID) error
	// revokeCredentials stamps revoked_at on the source's still-active
	// credentials only.
	revokeCredentials(ctx context.Context, workspaceID, sourceID ID) error
}

// archive runs the fixed order: lock the source, archive it, revoke its active
// credentials. Credential issue and rotate take the same first lock, so no
// active credential can appear after the caller commits. Any error means the
// caller must roll back the whole transaction.
func archive(ctx context.Context, tx archiveTx, workspaceID, sourceID ID) error {
	found, err := tx.lockSource(ctx, workspaceID, sourceID)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if err := tx.markArchived(ctx, workspaceID, sourceID); err != nil {
		return err
	}
	return tx.revokeCredentials(ctx, workspaceID, sourceID)
}
