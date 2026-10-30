// Package workspaces owns workspace membership: the single role lookup used by
// authorisation callers and the personal workspace created together with a new
// account. It never begins, commits or rolls back a transaction: creation always
// runs inside the caller's transaction, so a half-written account, workspace or
// membership is impossible.
package workspaces

import (
	"context"
	"errors"

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

// personalName is the fixed display name of the initial personal workspace. It is
// never taken from a request, so registration cannot choose a name or a type.
const personalName = "Personal"

var (
	// ErrUserNotFound reports a create request for a user id that does not exist.
	// No workspace is written in that case.
	ErrUserNotFound = errors.New("workspaces: user not found")
	// ErrUnknownRole reports a stored role outside the owner/member vocabulary.
	// Callers must deny access instead of treating it as a known role.
	ErrUnknownRole = errors.New("workspaces: unknown role")
)

// Workspace carries only the identity and display name the current callers need.
type Workspace struct {
	ID   ID
	Name string
}

// RoleStore is the pool-bound membership read surface. It answers one
// (user, workspace) pair and never enumerates members or workspaces.
type RoleStore interface {
	// MembershipRole returns the stored role for the pair. found=false means the
	// user is not a member; a database failure is a real error, never a miss.
	MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error)
	// CurrentWorkspace returns the earliest workspace the user owns. found=false
	// means the user owns none; a database failure is a real error.
	CurrentWorkspace(ctx context.Context, userID ID) (Workspace, bool, error)
}

// createStore is the transaction-bound persistence surface of personal workspace
// creation. Only a store bound to an open pgx.Tx can satisfy it, so creation can
// never run on a pooled connection where the users row lock would be released
// after a single statement. Every step is unexported: callers outside this package
// can only reach the fixed order through Tx.CreatePersonal, so no single step can
// be run out of order.
type createStore interface {
	// lockUser locks the users row for the duration of the transaction and
	// reports whether it exists. This serialises creation per user.
	lockUser(ctx context.Context, userID ID) (bool, error)
	// personalWorkspace returns the initial workspace the user already owns, if
	// any, as the owner membership row records it.
	personalWorkspace(ctx context.Context, userID ID) (Workspace, bool, error)
	// insertWorkspace stores one workspace and returns its database identity.
	insertWorkspace(ctx context.Context, name string) (Workspace, error)
	// insertOwnerMembership stores the owner membership of that workspace.
	insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error
}

// Service answers membership role questions on the caller-owned pool. T-010 and
// T-021 must use it instead of querying workspace_memberships themselves.
type Service struct {
	roles RoleStore
}

// New returns a Service backed by the given pool-bound store.
func New(roles RoleStore) *Service {
	return &Service{roles: roles}
}

// MembershipRole returns the role of one user in one workspace. A non-member is
// reported as found=false without an error so it stays a normal authorisation
// input. A database failure or a stored role outside owner/member is returned as
// an error, so it can never be mistaken for a non-member.
func (s *Service) MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error) {
	role, found, err := s.roles.MembershipRole(ctx, userID, workspaceID)
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

// CurrentWorkspace returns the personal workspace of the user: the earliest
// workspace the user owns. found=false means none exists, which is a data
// integrity fault for a registered user and must never be answered with a
// created or invented workspace. It only holds while every user has a single
// personal workspace and must be redesigned before multiple workspaces exist.
func (s *Service) CurrentWorkspace(ctx context.Context, userID ID) (Workspace, bool, error) {
	return s.roles.CurrentWorkspace(ctx, userID)
}

// createPersonal runs the documented creation sequence on a transaction-bound
// store: lock the user row, look up the initial workspace the user already owns,
// and only then insert the workspace and its owner membership. Repeat and
// concurrent calls therefore observe the first committed result instead of
// creating a second workspace. It never commits, rolls back or retries; any error
// means the caller must roll back the whole transaction.
func createPersonal(ctx context.Context, store createStore, userID ID) (Workspace, error) {
	exists, err := store.lockUser(ctx, userID)
	if err != nil {
		return Workspace{}, err
	}
	if !exists {
		return Workspace{}, ErrUserNotFound
	}
	existing, found, err := store.personalWorkspace(ctx, userID)
	if err != nil {
		return Workspace{}, err
	}
	if found {
		return existing, nil
	}
	created, err := store.insertWorkspace(ctx, personalName)
	if err != nil {
		return Workspace{}, err
	}
	if err := store.insertOwnerMembership(ctx, created.ID, userID); err != nil {
		return Workspace{}, err
	}
	return created, nil
}
