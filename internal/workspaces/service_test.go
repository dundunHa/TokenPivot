package workspaces

import (
	"context"
	"errors"
	"testing"
)

// call records one fake store method invocation so ordering can be asserted.
type call string

// fakeCreateStore is an in-memory stand-in for the transaction-bound creation
// surface. It records call order and lets a test inject failures per step.
type fakeCreateStore struct {
	users map[[16]byte]bool
	// owners maps a user id to the workspace that user already owns.
	owners map[[16]byte]Workspace
	names  map[[16]byte]string
	calls  []call

	lockErr    error
	lookupErr  error
	insertErr  error
	memberErr  error
	nextID     byte
	insertedWS int
	insertedMB int
}

func newFakeCreateStore() *fakeCreateStore {
	return &fakeCreateStore{
		users:  make(map[[16]byte]bool),
		owners: make(map[[16]byte]Workspace),
		names:  make(map[[16]byte]string),
	}
}

func (f *fakeCreateStore) addUser(id ID) {
	f.users[id.Bytes] = true
}

func (f *fakeCreateStore) LockUser(ctx context.Context, userID ID) (bool, error) {
	f.calls = append(f.calls, "lock")
	if f.lockErr != nil {
		return false, f.lockErr
	}
	return f.users[userID.Bytes], nil
}

func (f *fakeCreateStore) PersonalWorkspace(ctx context.Context, userID ID) (Workspace, bool, error) {
	f.calls = append(f.calls, "lookup")
	if f.lookupErr != nil {
		return Workspace{}, false, f.lookupErr
	}
	workspace, ok := f.owners[userID.Bytes]
	return workspace, ok, nil
}

func (f *fakeCreateStore) InsertWorkspace(ctx context.Context, name string) (Workspace, error) {
	f.calls = append(f.calls, "insert workspace")
	if f.insertErr != nil {
		return Workspace{}, f.insertErr
	}
	f.nextID++
	workspace := Workspace{ID: id(f.nextID), Name: name}
	f.names[workspace.ID.Bytes] = name
	f.insertedWS++
	return workspace, nil
}

func (f *fakeCreateStore) InsertOwnerMembership(ctx context.Context, workspaceID, userID ID) error {
	f.calls = append(f.calls, "insert membership")
	if f.memberErr != nil {
		return f.memberErr
	}
	f.owners[userID.Bytes] = Workspace{ID: workspaceID, Name: f.names[workspaceID.Bytes]}
	f.insertedMB++
	return nil
}

// fakeRoleStore is the pool-bound read stand-in.
type fakeRoleStore struct {
	rows   map[[16]byte]map[[16]byte]Role
	err    error
	answer *Role
}

func (f *fakeRoleStore) MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	if f.answer != nil {
		return *f.answer, true, nil
	}
	byWorkspace, ok := f.rows[userID.Bytes]
	if !ok {
		return "", false, nil
	}
	role, ok := byWorkspace[workspaceID.Bytes]
	return role, ok, nil
}

func id(tag byte) ID {
	return ID{Bytes: [16]byte{0x03, tag}, Valid: true}
}

func TestCreatePersonalWritesWorkspaceAndOwnerMembership(t *testing.T) {
	store := newFakeCreateStore()
	userID := id(0x01)
	store.addUser(userID)

	workspace, err := createPersonal(context.Background(), store, userID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != personalName {
		t.Fatalf("name=%q want=%q", workspace.Name, personalName)
	}
	if !workspace.ID.Valid {
		t.Fatal("workspace id was not assigned")
	}
	if store.insertedWS != 1 || store.insertedMB != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", store.insertedWS, store.insertedMB)
	}
	if owner := store.owners[userID.Bytes]; owner.ID.Bytes != workspace.ID.Bytes {
		t.Fatal("owner membership does not point at the created workspace")
	}
	want := []call{"lock", "lookup", "insert workspace", "insert membership"}
	if len(store.calls) != len(want) {
		t.Fatalf("calls=%v want=%v", store.calls, want)
	}
	for i, name := range want {
		if store.calls[i] != name {
			t.Fatalf("call %d=%q want=%q (order is the concurrency guarantee)", i, store.calls[i], name)
		}
	}
}

// TestCreatePersonalRepeatReturnsSameWorkspace covers the idempotent second call:
// it re-reads after the lock and writes nothing.
func TestCreatePersonalRepeatReturnsSameWorkspace(t *testing.T) {
	store := newFakeCreateStore()
	userID := id(0x02)
	store.addUser(userID)
	first, err := createPersonal(context.Background(), store, userID)
	if err != nil {
		t.Fatal(err)
	}
	store.calls = nil
	second, err := createPersonal(context.Background(), store, userID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID.Bytes != first.ID.Bytes || second.Name != first.Name {
		t.Fatal("the repeated call returned a different workspace")
	}
	if store.insertedWS != 1 || store.insertedMB != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", store.insertedWS, store.insertedMB)
	}
	if len(store.calls) != 2 || store.calls[0] != "lock" || store.calls[1] != "lookup" {
		t.Fatalf("repeated calls=%v want only lock and lookup", store.calls)
	}
}

func TestCreatePersonalUnknownUserWritesNothing(t *testing.T) {
	store := newFakeCreateStore()
	_, err := createPersonal(context.Background(), store, id(0x03))
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error=%v want ErrUserNotFound", err)
	}
	if store.insertedWS != 0 || store.insertedMB != 0 {
		t.Fatal("an unknown user created a workspace")
	}
}

func TestCreatePersonalPropagatesFailures(t *testing.T) {
	boom := errors.New("synthetic-database-failure")
	cases := []struct {
		name  string
		store *fakeCreateStore
	}{
		{"user lock", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x04))
			s.lockErr = boom
			return s
		}()},
		{"owner lookup", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x04))
			s.lookupErr = boom
			return s
		}()},
		{"workspace insert", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x04))
			s.insertErr = boom
			return s
		}()},
		{"membership insert", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x04))
			s.memberErr = boom
			return s
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createPersonal(context.Background(), tc.store, id(0x04))
			if !errors.Is(err, boom) {
				t.Fatalf("error=%v want the injected failure", err)
			}
			// No success is reported, so the caller rolls the whole transaction
			// back and cannot commit a half-written pair.
			if tc.store.insertedMB != 0 {
				t.Fatal("a failed creation reported a committed membership")
			}
		})
	}
}

func TestMembershipRoleReturnsKnownRoles(t *testing.T) {
	userID, workspaceID := id(0x05), id(0x06)
	store := &fakeRoleStore{rows: map[[16]byte]map[[16]byte]Role{
		userID.Bytes: {workspaceID.Bytes: RoleOwner},
	}}
	role, found, err := New(store).MembershipRole(context.Background(), userID, workspaceID)
	if err != nil || !found || role != RoleOwner {
		t.Fatalf("role=%q found=%t err=%v want owner and found", role, found, err)
	}

	member := RoleMember
	store.answer = &member
	role, found, err = New(store).MembershipRole(context.Background(), userID, workspaceID)
	if err != nil || !found || role != RoleMember {
		t.Fatalf("role=%q found=%t err=%v want member and found", role, found, err)
	}
}

func TestMembershipRoleNonMemberIsNotFoundNotAnError(t *testing.T) {
	store := &fakeRoleStore{rows: map[[16]byte]map[[16]byte]Role{}}
	role, found, err := New(store).MembershipRole(context.Background(), id(0x07), id(0x08))
	if err != nil {
		t.Fatalf("non-member returned an error: %v", err)
	}
	if found || role != "" {
		t.Fatalf("role=%q found=%t want empty and false", role, found)
	}
}

func TestMembershipRoleUnknownRoleIsAnError(t *testing.T) {
	unknown := Role("superuser")
	store := &fakeRoleStore{answer: &unknown}
	role, found, err := New(store).MembershipRole(context.Background(), id(0x09), id(0x0a))
	if !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("error=%v want ErrUnknownRole", err)
	}
	// The caller must deny: no role and no found=true leaks out.
	if found || role != "" {
		t.Fatalf("role=%q found=%t want empty and false", role, found)
	}
}

func TestMembershipRoleDatabaseFailureIsNotAMiss(t *testing.T) {
	boom := errors.New("synthetic-database-failure")
	_, found, err := New(&fakeRoleStore{err: boom}).MembershipRole(context.Background(), id(0x0b), id(0x0c))
	if !errors.Is(err, boom) {
		t.Fatalf("error=%v want the database failure", err)
	}
	if found {
		t.Fatal("a database failure was reported as a membership")
	}
}

// TestRolesAreTheDocumentedVocabulary pins the two accepted values: any other
// text in the role column must be rejected rather than weakened.
func TestRolesAreTheDocumentedVocabulary(t *testing.T) {
	if RoleOwner != "owner" || RoleMember != "member" {
		t.Fatalf("roles=%q,%q want owner,member", RoleOwner, RoleMember)
	}
}
