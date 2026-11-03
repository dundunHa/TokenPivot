package workspaces

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// call records one fake store method invocation so ordering can be asserted.
type call string

// fakeCreateStore is an in-memory stand-in for the transaction-bound creation
// surface. It records call order and lets a test inject failures per step.
type fakeCreateStore struct {
	users map[[16]byte]bool
	// created counts the workspaces each account already created.
	created map[[16]byte]int
	// taken records the names each account already used.
	taken map[[16]byte]map[string]bool
	calls []call

	lockErr   error
	countErr  error
	insertErr error
	memberErr error

	nextID     byte
	insertedWS int
	insertedMB int
	owner      map[[16]byte]Workspace
	names      map[[16]byte]string
}

func newFakeCreateStore() *fakeCreateStore {
	return &fakeCreateStore{
		users:   make(map[[16]byte]bool),
		created: make(map[[16]byte]int),
		taken:   make(map[[16]byte]map[string]bool),
		owner:   make(map[[16]byte]Workspace),
		names:   make(map[[16]byte]string),
	}
}

func (f *fakeCreateStore) addUser(userID ID) {
	f.users[userID.Bytes] = true
}

func (f *fakeCreateStore) countFor(userID ID, created int) {
	f.created[userID.Bytes] = created
}

func (f *fakeCreateStore) takeName(userID ID, name string) {
	if f.taken[userID.Bytes] == nil {
		f.taken[userID.Bytes] = make(map[string]bool)
	}
	f.taken[userID.Bytes][name] = true
}

func (f *fakeCreateStore) lockUser(ctx context.Context, userID ID) (bool, error) {
	f.calls = append(f.calls, "lock")
	if f.lockErr != nil {
		return false, f.lockErr
	}
	return f.users[userID.Bytes], nil
}

func (f *fakeCreateStore) countCreatedBy(ctx context.Context, userID ID) (int, error) {
	f.calls = append(f.calls, "count")
	if f.countErr != nil {
		return 0, f.countErr
	}
	return f.created[userID.Bytes], nil
}

func (f *fakeCreateStore) insertWorkspace(ctx context.Context, userID ID, name string) (Workspace, error) {
	f.calls = append(f.calls, "insert workspace")
	if f.insertErr != nil {
		return Workspace{}, f.insertErr
	}
	if f.taken[userID.Bytes][name] {
		return Workspace{}, ErrNameConflict
	}
	f.nextID++
	workspace := Workspace{ID: id(f.nextID), Name: name}
	f.names[workspace.ID.Bytes] = name
	f.takeName(userID, name)
	f.created[userID.Bytes]++
	f.insertedWS++
	return workspace, nil
}

func (f *fakeCreateStore) insertOwnerMembership(ctx context.Context, workspaceID, userID ID) error {
	f.calls = append(f.calls, "insert membership")
	if f.memberErr != nil {
		return f.memberErr
	}
	f.owner[userID.Bytes] = Workspace{ID: workspaceID, Name: f.names[workspaceID.Bytes]}
	f.insertedMB++
	return nil
}

// fakeTx runs the creation function on the fake store, so a test can assert that
// validation happens before any transaction is opened.
type fakeTx struct {
	store *fakeCreateStore
	err   error
	calls int
}

func (f *fakeTx) InTx(ctx context.Context, fn func(context.Context, createStore) error) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return fn(ctx, f.store)
}

// fakeStore is the pool-bound read stand-in.
type fakeStore struct {
	roles  map[[16]byte]map[[16]byte]Role
	err    error
	answer *Role

	list    []Workspace
	listErr error

	byID    map[[16]byte]Workspace
	byIDErr error
}

func (f *fakeStore) MembershipRole(ctx context.Context, userID, workspaceID ID) (Role, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	if f.answer != nil {
		return *f.answer, true, nil
	}
	byWorkspace, ok := f.roles[userID.Bytes]
	if !ok {
		return "", false, nil
	}
	role, ok := byWorkspace[workspaceID.Bytes]
	return role, ok, nil
}

func (f *fakeStore) List(ctx context.Context, userID ID) ([]Workspace, error) {
	return f.list, f.listErr
}

func (f *fakeStore) Workspace(ctx context.Context, workspaceID ID) (Workspace, bool, error) {
	if f.byIDErr != nil {
		return Workspace{}, false, f.byIDErr
	}
	workspace, ok := f.byID[workspaceID.Bytes]
	return workspace, ok, nil
}

func id(tag byte) ID {
	return ID{Bytes: [16]byte{0x03, tag}, Valid: true}
}

func TestCreateWritesWorkspaceAndOwnerMembership(t *testing.T) {
	store := newFakeCreateStore()
	userID := id(0x01)
	store.addUser(userID)
	tx := &fakeTx{store: store}

	workspace, err := New(&fakeStore{}, tx).Create(context.Background(), userID, "  Alpha  ")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Name != "Alpha" {
		t.Fatalf("name=%q want the trimmed name Alpha", workspace.Name)
	}
	if !workspace.ID.Valid {
		t.Fatal("workspace id was not assigned")
	}
	if store.insertedWS != 1 || store.insertedMB != 1 {
		t.Fatalf("workspaces=%d memberships=%d want 1 and 1", store.insertedWS, store.insertedMB)
	}
	if owner := store.owner[userID.Bytes]; owner.ID.Bytes != workspace.ID.Bytes {
		t.Fatal("owner membership does not point at the created workspace")
	}
	want := []call{"lock", "count", "insert workspace", "insert membership"}
	if len(store.calls) != len(want) {
		t.Fatalf("calls=%v want=%v", store.calls, want)
	}
	for i, name := range want {
		if store.calls[i] != name {
			t.Fatalf("call %d=%q want=%q (lock then count is the quota guarantee)", i, store.calls[i], name)
		}
	}
}

func TestCreateUnknownUserWritesNothing(t *testing.T) {
	store := newFakeCreateStore()
	_, err := New(&fakeStore{}, &fakeTx{store: store}).Create(context.Background(), id(0x03), "Alpha")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error=%v want ErrUserNotFound", err)
	}
	if store.insertedWS != 0 || store.insertedMB != 0 {
		t.Fatal("an unknown user created a workspace")
	}
}

func TestCreateAtLimitWritesNothing(t *testing.T) {
	store := newFakeCreateStore()
	userID := id(0x04)
	store.addUser(userID)
	store.countFor(userID, freeWorkspaceLimit)

	_, err := New(&fakeStore{}, &fakeTx{store: store}).Create(context.Background(), userID, "Third")
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("error=%v want ErrLimitReached", err)
	}
	if store.insertedWS != 0 || store.insertedMB != 0 {
		t.Fatal("an account at its limit created a workspace")
	}
	if len(store.calls) != 2 || store.calls[0] != "lock" || store.calls[1] != "count" {
		t.Fatalf("calls=%v want only lock and count", store.calls)
	}
}

func TestCreateDuplicateNameWritesNoMembership(t *testing.T) {
	store := newFakeCreateStore()
	userID := id(0x05)
	store.addUser(userID)
	store.takeName(userID, "Alpha")

	_, err := New(&fakeStore{}, &fakeTx{store: store}).Create(context.Background(), userID, "Alpha")
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("error=%v want ErrNameConflict", err)
	}
	// The workspace insert is the only failing step, so no owner row exists.
	if store.insertedMB != 0 {
		t.Fatal("a conflicting name reported a committed membership")
	}
}

// TestCreatePropagatesFailures pins that every step's failure reaches the caller,
// so the surrounding transaction is rolled back instead of committing a
// workspace without its owner membership.
func TestCreatePropagatesFailures(t *testing.T) {
	boom := errors.New("synthetic-database-failure")
	cases := []struct {
		name  string
		store *fakeCreateStore
	}{
		{"user lock", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x06))
			s.lockErr = boom
			return s
		}()},
		{"created count", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x06))
			s.countErr = boom
			return s
		}()},
		{"workspace insert", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x06))
			s.insertErr = boom
			return s
		}()},
		{"membership insert", func() *fakeCreateStore {
			s := newFakeCreateStore()
			s.addUser(id(0x06))
			s.memberErr = boom
			return s
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(&fakeStore{}, &fakeTx{store: tc.store}).Create(context.Background(), id(0x06), "Alpha")
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

func TestCreateTransactionFailurePropagates(t *testing.T) {
	boom := errors.New("synthetic-transaction-failure")
	store := newFakeCreateStore()
	userID := id(0x07)
	store.addUser(userID)
	tx := &fakeTx{store: store, err: boom}

	_, err := New(&fakeStore{}, tx).Create(context.Background(), userID, "Alpha")
	if !errors.Is(err, boom) {
		t.Fatalf("error=%v want the transaction failure", err)
	}
	if len(store.calls) != 0 {
		t.Fatalf("calls=%v want none when the transaction cannot start", store.calls)
	}
}

// TestNormalizeNameTable covers the documented rule: trimming, the empty result,
// both length limits, control characters, inner whitespace and the deliberate
// absence of case folding or Unicode normalization.
func TestNormalizeNameTable(t *testing.T) {
	byteBoundary := strings.Repeat("\U0001F600", 64)  // 64 code points, 256 bytes
	overBytes := byteBoundary + "a"                   // 64 code points, 257 bytes
	overRunes := strings.Repeat("a", nameMaxRunes+1)  // 81 code points
	runeBoundary := strings.Repeat("a", nameMaxRunes) // 80 code points
	wideBoundary := strings.Repeat("\u20AC", 80)      // 80 code points, 240 bytes
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"plain", "Alpha", "Alpha", true},
		{"trimmed", "\t Alpha \n", "Alpha", true},
		{"trimmed wide space", "\u3000Alpha\u3000", "Alpha", true},
		{"inner whitespace", "Alpha Beta", "Alpha Beta", true},
		{"empty", "", "", false},
		{"only whitespace", "   ", "", false},
		{"only non-breaking space", "\u00a0", "", false},
		{"rune boundary", runeBoundary, runeBoundary, true},
		{"over rune limit", overRunes, "", false},
		{"wide rune boundary", wideBoundary, wideBoundary, true},
		{"byte boundary", byteBoundary, byteBoundary, true},
		{"over byte limit", overBytes, "", false},
		{"nul control", "Al\x00pha", "", false},
		{"tab control in middle", "Al\tpha", "", false},
		{"del control", "Al\x7fpha", "", false},
		// Case and normalization are not folded: both spellings stay distinct.
		{"lowercase", "alpha", "alpha", true},
		{"uppercase", "ALPHA", "ALPHA", true},
		{"decomposed", "A\u0308lpha", "A\u0308lpha", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeName(tc.input)
			if tc.ok {
				if err != nil {
					t.Fatalf("error=%v want the name accepted", err)
				}
				if got != tc.want {
					t.Fatalf("name=%q want=%q", got, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Fatalf("error=%v want ErrInvalidName", err)
			}
		})
	}
}

// TestCreateRejectsInvalidNameBeforeAnyTransaction proves validation happens
// outside the transaction: no connection is opened for a name that cannot be
// stored.
func TestCreateRejectsInvalidNameBeforeAnyTransaction(t *testing.T) {
	for _, name := range []string{"", "   ", "\x00", strings.Repeat("a", nameMaxRunes+1)} {
		store := newFakeCreateStore()
		userID := id(0x08)
		store.addUser(userID)
		tx := &fakeTx{store: store}
		if _, err := New(&fakeStore{}, tx).Create(context.Background(), userID, name); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name=%q error=%v want ErrInvalidName", name, err)
		}
		if tx.calls != 0 || len(store.calls) != 0 {
			t.Fatalf("name=%q opened a transaction or touched the store", name)
		}
	}
}

func TestListPassesThrough(t *testing.T) {
	want := []Workspace{{ID: id(0x0a), Name: "Alpha"}, {ID: id(0x0b), Name: "Beta"}}
	store := &fakeStore{list: want}
	got, err := New(store, &fakeTx{store: newFakeCreateStore()}).List(context.Background(), id(0x01))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("list=%v want the store order unchanged", got)
	}
}

func TestListPropagatesError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := New(&fakeStore{listErr: boom}, &fakeTx{store: newFakeCreateStore()}).List(context.Background(), id(0x01)); !errors.Is(err, boom) {
		t.Fatalf("error=%v want the store failure", err)
	}
}

func TestWorkspaceLookupPassesThrough(t *testing.T) {
	want := Workspace{ID: id(0x0c), Name: "Alpha"}
	service := New(&fakeStore{byID: map[[16]byte]Workspace{want.ID.Bytes: want}}, &fakeTx{store: newFakeCreateStore()})
	got, found, err := service.Workspace(context.Background(), want.ID)
	if err != nil || !found || got != want {
		t.Fatalf("got=%+v found=%t err=%v", got, found, err)
	}
	if _, found, err := service.Workspace(context.Background(), id(0x0d)); err != nil || found {
		t.Fatalf("missing found=%t err=%v", found, err)
	}
	boom := errors.New("boom")
	if _, _, err := New(&fakeStore{byIDErr: boom}, &fakeTx{store: newFakeCreateStore()}).Workspace(context.Background(), want.ID); !errors.Is(err, boom) {
		t.Fatalf("error=%v want the store failure", err)
	}
}

func TestMembershipRoleReturnsKnownRoles(t *testing.T) {
	userID, workspaceID := id(0x05), id(0x06)
	store := &fakeStore{roles: map[[16]byte]map[[16]byte]Role{
		userID.Bytes: {workspaceID.Bytes: RoleOwner},
	}}
	role, found, err := New(store, &fakeTx{store: newFakeCreateStore()}).MembershipRole(context.Background(), userID, workspaceID)
	if err != nil || !found || role != RoleOwner {
		t.Fatalf("role=%q found=%t err=%v want owner and found", role, found, err)
	}

	member := RoleMember
	store.answer = &member
	role, found, err = New(store, &fakeTx{store: newFakeCreateStore()}).MembershipRole(context.Background(), userID, workspaceID)
	if err != nil || !found || role != RoleMember {
		t.Fatalf("role=%q found=%t err=%v want member and found", role, found, err)
	}
}

func TestMembershipRoleNonMemberIsNotFoundNotAnError(t *testing.T) {
	store := &fakeStore{roles: map[[16]byte]map[[16]byte]Role{}}
	role, found, err := New(store, &fakeTx{store: newFakeCreateStore()}).MembershipRole(context.Background(), id(0x07), id(0x08))
	if err != nil {
		t.Fatalf("non-member returned an error: %v", err)
	}
	if found || role != "" {
		t.Fatalf("role=%q found=%t want empty and false", role, found)
	}
}

func TestMembershipRoleUnknownRoleIsAnError(t *testing.T) {
	unknown := Role("superuser")
	store := &fakeStore{answer: &unknown}
	role, found, err := New(store, &fakeTx{store: newFakeCreateStore()}).MembershipRole(context.Background(), id(0x09), id(0x0a))
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
	_, found, err := New(&fakeStore{err: boom}, &fakeTx{store: newFakeCreateStore()}).MembershipRole(context.Background(), id(0x0b), id(0x0c))
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

// TestIsNameConflictOnlyAcceptsTheWorkspaceNameIndex proves a unique violation
// from any other index (the membership pair, a future index) is not reported to
// the user as a duplicate workspace name.
func TestIsNameConflictOnlyAcceptsTheWorkspaceNameIndex(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"workspace name index", &pgconn.PgError{Code: "23505", ConstraintName: workspaceNameIndex}, true},
		{"membership pair index", &pgconn.PgError{Code: "23505", ConstraintName: "uk_workspace_memberships_workspace_id_user_id"}, false},
		{"other unique index", &pgconn.PgError{Code: "23505", ConstraintName: "uk_workspaces_pkey"}, false},
		{"other sqlstate", &pgconn.PgError{Code: "23503", ConstraintName: workspaceNameIndex}, false},
		{"not a database error", errors.New("synthetic"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNameConflict(tc.err); got != tc.want {
				t.Fatalf("isNameConflict=%t want=%t", got, tc.want)
			}
		})
	}
}

// TestFreeWorkspaceLimit pins the business constant the quota is counted against.
func TestFreeWorkspaceLimit(t *testing.T) {
	if freeWorkspaceLimit != 2 {
		t.Fatalf("freeWorkspaceLimit=%d want 2", freeWorkspaceLimit)
	}
}
