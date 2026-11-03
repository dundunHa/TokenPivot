// Package workspaces owns workspace membership and the workspace lifecycle: the
// single role lookup used by authorisation callers, and manual workspace
// creation, listing and lookup. A workspace is created by one account and owned
// by it through a membership row; the creator column only counts toward that
// account's quota and scopes the unique name, never the authorisation.
package workspaces

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// ID is the PostgreSQL uuid identity of a user or workspace.
type ID = pgtype.UUID

// Role is the application-validated membership role. Only these two values are
// accepted; anything else is a data error and never degrades to a weaker role.
type Role string

const (
	// RoleOwner may manage a workspace and everything it owns.
	RoleOwner Role = "owner"
	// RoleMember may read a workspace and everything it owns.
	RoleMember Role = "member"
)

const (
	// freeWorkspaceLimit is the number of workspaces one account may create. It
	// is a business constant, not a plan or configuration: the first workspace
	// counts too.
	freeWorkspaceLimit = 2
	// nameMaxRunes and nameMaxBytes bound one display name. Both limits apply;
	// a name may not exceed either one.
	nameMaxRunes = 80
	nameMaxBytes = 256
)

var (
	// ErrUserNotFound reports a create request for a user id that does not exist.
	// No workspace is written in that case.
	ErrUserNotFound = errors.New("workspaces: user not found")
	// ErrUnknownRole reports a stored role outside the owner/member vocabulary.
	// Callers must deny access instead of treating it as a known role.
	ErrUnknownRole = errors.New("workspaces: unknown role")
	// ErrInvalidName reports a name that is empty after trimming, too long or
	// contains a control character. Nothing is written.
	ErrInvalidName = errors.New("workspaces: invalid name")
	// ErrNameConflict reports a name the creating account already used. The
	// comparison is exact bytes after trimming: case and Unicode normalization
	// are never folded.
	ErrNameConflict = errors.New("workspaces: name conflict")
	// ErrLimitReached reports an account that already created its free
	// workspaces. Nothing is written.
	ErrLimitReached = errors.New("workspaces: limit reached")
)

// Workspace carries only the identity and display name the current callers need.
type Workspace struct {
	ID   ID
	Name string
}

// Store is the pool-bound read surface. It answers one (user, workspace) pair,
// lists the workspaces one user belongs to and reads one workspace by id.
type Store interface {
	// MembershipRole returns the stored role for the pair. found=false means the
	// user is not a member; a database failure is a real error, never a miss.
	MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error)
	// List returns every workspace the user is a member of, ordered by name in
	// C collation and then by id. No membership yields an empty non-nil slice.
	List(ctx context.Context, userID ID) ([]Workspace, error)
	// Workspace returns one workspace by id. found=false means no such row. It
	// does not authorise anything: callers must resolve MembershipRole first.
	Workspace(ctx context.Context, workspaceID ID) (Workspace, bool, error)
}

// createStore is the transaction-bound persistence surface of workspace
// creation. Only a store bound to an open transaction can satisfy it, so the
// users row lock outlives a single statement and the workspace and its owner
// membership commit or roll back together. Every step is unexported: callers
// outside this package can only reach the fixed order through TxRunner.InTx, so
// no single step can be run out of order.
type createStore interface {
	// lockUser locks the users row for the duration of the transaction and
	// reports whether it exists. This serialises creation per account.
	lockUser(ctx context.Context, userID ID) (bool, error)
	// countCreatedBy counts the workspaces the account already created.
	countCreatedBy(ctx context.Context, userID ID) (int, error)
	// insertWorkspace stores one workspace and returns its database identity.
	insertWorkspace(ctx context.Context, userID ID, name string) (Workspace, error)
	// insertOwnerMembership stores the owner membership of that workspace.
	insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error
}

// TxRunner runs a function inside one READ COMMITTED database transaction. The
// store handed to the function is bound to that transaction, so the workspace and
// its owner membership commit or roll back as a unit. It is implemented by this
// package's PostgreSQL adapter; the unexported store keeps the fixed creation
// order the only reachable path.
type TxRunner interface {
	InTx(ctx context.Context, fn func(context.Context, createStore) error) error
}

// Service answers membership role questions and the workspace use cases. T-010
// and T-021 must use it instead of querying workspace_memberships themselves.
type Service struct {
	store Store
	tx    TxRunner
}

// New returns a Service. Reads outside a transaction use store; Create uses tx so
// the quota check, the workspace row and its owner membership are one atomic
// unit.
func New(store Store, tx TxRunner) *Service {
	return &Service{store: store, tx: tx}
}

// MembershipRole returns the role of one user in one workspace. A non-member is
// reported as found=false without an error so it stays a normal authorisation
// input. A database failure or a stored role outside owner/member is returned as
// an error, so it can never be mistaken for a non-member.
func (s *Service) MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error) {
	role, found, err := s.store.MembershipRole(ctx, userID, workspaceID)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	switch role {
	case RoleOwner, RoleMember:
		return role, true, nil
	default:
		return "", false, ErrUnknownRole
	}
}

// List returns every workspace the user is a member of, ordered by name in C
// collation and then by id. The order is decided by the database so every client
// shows the same sequence. No membership is a normal empty slice, not an error.
func (s *Service) List(ctx context.Context, userID ID) ([]Workspace, error) {
	return s.store.List(ctx, userID)
}

// Workspace returns one workspace by id. found=false means no such row. It
// performs no authorisation: a caller must resolve MembershipRole for the same
// user and workspace before asking, so a missing row after a granted membership
// is a data integrity fault rather than a permission decision.
func (s *Service) Workspace(ctx context.Context, workspaceID ID) (Workspace, bool, error) {
	return s.store.Workspace(ctx, workspaceID)
}

// Create stores one workspace for the account and makes it the owner. The name
// is trimmed and validated before any database work. Inside one transaction the
// users row is locked, the account's created workspaces are counted, and only
// then are the workspace and its owner membership inserted; repeat calls above
// freeWorkspaceLimit fail with ErrLimitReached and a name the account already
// used fails with ErrNameConflict from the unique index.
func (s *Service) Create(ctx context.Context, userID ID, name string) (Workspace, error) {
	trimmed, err := normalizeName(name)
	if err != nil {
		return Workspace{}, err
	}
	var created Workspace
	err = s.tx.InTx(ctx, func(ctx context.Context, tx createStore) error {
		workspace, err := createWorkspace(ctx, tx, userID, trimmed)
		if err != nil {
			return err
		}
		created = workspace
		return nil
	})
	if err != nil {
		return Workspace{}, err
	}
	return created, nil
}

// createWorkspace runs the fixed creation order on a transaction-bound store:
// lock the user row, count the workspaces that account created, insert the
// workspace and then its owner membership. Under READ COMMITTED a second
// transaction for the same account waits on the row lock and then sees the first
// committed count, so at most freeWorkspaceLimit creations succeed. It never
// checks names beforehand: the unique index is the only arbiter. It never
// commits, rolls back or retries; any error means the caller must roll back the
// whole transaction.
func createWorkspace(ctx context.Context, store createStore, userID ID, name string) (Workspace, error) {
	exists, err := store.lockUser(ctx, userID)
	if err != nil {
		return Workspace{}, err
	}
	if !exists {
		return Workspace{}, ErrUserNotFound
	}
	created, err := store.countCreatedBy(ctx, userID)
	if err != nil {
		return Workspace{}, err
	}
	if created >= freeWorkspaceLimit {
		return Workspace{}, ErrLimitReached
	}
	workspace, err := store.insertWorkspace(ctx, userID, name)
	if err != nil {
		return Workspace{}, err
	}
	if err := store.insertOwnerMembership(ctx, workspace.ID, userID); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

// normalizeName applies the display name rule and returns the trimmed name that
// is stored: leading and trailing Unicode whitespace removed, 1–80 code points,
// at most 256 UTF-8 bytes, no control character. Inner whitespace is allowed. It
// never folds case and never normalizes Unicode, so "Alpha" and "alpha" are
// different names.
func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", ErrInvalidName
	}
	if utf8.RuneCountInString(name) > nameMaxRunes || len(name) > nameMaxBytes {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	return name, nil
}
