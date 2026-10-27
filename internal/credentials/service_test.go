package credentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory store. It mirrors only the SQL semantics the
// service relies on and records what the service wrote.
type fakeStore struct {
	mu sync.Mutex
	// sources maps source id to owning workspace id.
	sources map[[16]byte][16]byte
	// rows holds one stored credential per id, keyed by credential id.
	rows map[[16]byte]storedCredential
	// digests records every hash handed to Insert, in insertion order.
	digests [][]byte

	errList     error
	errByHash   error
	errByID     error
	errSource   error
	errInsert   error
	errRevoke   error
	revokeCalls int
}

type storedCredential struct {
	credential Credential
	hash       []byte
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sources: make(map[[16]byte][16]byte),
		rows:    make(map[[16]byte]storedCredential),
	}
}

func (f *fakeStore) addSource(workspaceID, sourceID ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources[sourceID.Bytes] = workspaceID.Bytes
}

func (f *fakeStore) List(ctx context.Context, workspaceID, sourceID ID) ([]Credential, error) {
	if f.errList != nil {
		return nil, f.errList
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]Credential, 0, len(f.rows))
	for _, row := range f.rows {
		if row.credential.WorkspaceID.Bytes == workspaceID.Bytes && row.credential.SourceID.Bytes == sourceID.Bytes {
			list = append(list, row.credential)
		}
	}
	return list, nil
}

func (f *fakeStore) BySecretHash(ctx context.Context, secretHash []byte) (Credential, bool, error) {
	if f.errByHash != nil {
		return Credential{}, false, f.errByHash
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range f.rows {
		if bytes.Equal(row.hash, secretHash) {
			return row.credential, true, nil
		}
	}
	return Credential{}, false, nil
}

func (f *fakeStore) ByID(ctx context.Context, workspaceID, credentialID ID) (Credential, bool, error) {
	if f.errByID != nil {
		return Credential{}, false, f.errByID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[credentialID.Bytes]
	if !ok || row.credential.WorkspaceID.Bytes != workspaceID.Bytes {
		return Credential{}, false, nil
	}
	return row.credential, true, nil
}

func (f *fakeStore) SourceExists(ctx context.Context, workspaceID, sourceID ID) (bool, error) {
	if f.errSource != nil {
		return false, f.errSource
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	owner, ok := f.sources[sourceID.Bytes]
	return ok && owner == workspaceID.Bytes, nil
}

func (f *fakeStore) Insert(ctx context.Context, workspaceID, sourceID ID, secretHash []byte) (Credential, error) {
	if f.errInsert != nil {
		return Credential{}, f.errInsert
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	credential := Credential{ID: nextID(), WorkspaceID: workspaceID, SourceID: sourceID}
	copied := append([]byte(nil), secretHash...)
	f.digests = append(f.digests, copied)
	f.rows[credential.ID.Bytes] = storedCredential{credential: credential, hash: copied}
	return credential, nil
}

func (f *fakeStore) Revoke(ctx context.Context, workspaceID, credentialID ID) (bool, error) {
	if f.errRevoke != nil {
		return false, f.errRevoke
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls++
	row, ok := f.rows[credentialID.Bytes]
	if !ok || row.credential.WorkspaceID.Bytes != workspaceID.Bytes {
		return false, nil
	}
	if row.credential.RevokedAt == nil {
		stamp := time.Now().UTC()
		row.credential.RevokedAt = &stamp
	}
	f.rows[credentialID.Bytes] = row
	return true, nil
}

// insertions returns how many rows the store accepted.
func (f *fakeStore) insertions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// stored returns the stored row for a credential id.
func (f *fakeStore) stored(t *testing.T, credentialID ID) storedCredential {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[credentialID.Bytes]
	if !ok {
		t.Fatal("credential is missing from the store")
	}
	return row
}

// sequence makes generated ids deterministic within a test run.
var sequence struct {
	mu   sync.Mutex
	next byte
}

func nextID() ID {
	sequence.mu.Lock()
	defer sequence.mu.Unlock()
	sequence.next++
	return ID{Bytes: [16]byte{0x01, sequence.next}, Valid: true}
}

// tokenDigest returns the raw digest of a token as a slice for comparisons.
func tokenDigest(token string) []byte {
	sum := secretHash(token)
	return sum[:]
}

func testID(tag byte) ID {
	return ID{Bytes: [16]byte{0x02, tag}, Valid: true}
}

func newTestService(t *testing.T) (*Service, *fakeStore, ID, ID) {
	t.Helper()
	fake := newFakeStore()
	workspaceID := testID(0x01)
	sourceID := testID(0x02)
	fake.addSource(workspaceID, sourceID)
	return New(fake), fake, workspaceID, sourceID
}

func TestIssueTokenPrefixAndUniqueness(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	first, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, issued := range []IssuedCredential{first, second} {
		if !strings.HasPrefix(issued.Token, tokenPrefix) {
			t.Fatalf("token prefix=%q want %q", issued.Token[:min(len(issued.Token), 8)], tokenPrefix)
		}
		if len(issued.Token) == len(tokenPrefix) {
			t.Fatal("token has no random suffix")
		}
	}
	if first.Token == second.Token {
		t.Fatal("two issued tokens are identical")
	}
	if first.ID.Bytes == second.ID.Bytes {
		t.Fatal("two issued credentials share an id")
	}
	if len(fake.digests) != 2 {
		t.Fatalf("stored digests=%d want=2", len(fake.digests))
	}
	for i, digest := range fake.digests {
		if len(digest) != sha256.Size {
			t.Fatalf("stored digest %d length=%d want=%d", i, len(digest), sha256.Size)
		}
	}
	if bytes.Contains(fake.digests[0], []byte(first.Token)) {
		t.Fatal("stored digest contains the plaintext token")
	}
	if !bytes.Equal(fake.digests[1], tokenDigest(second.Token)) {
		t.Fatal("stored digest is not the SHA-256 of the full token")
	}
}

func TestIssueReturnsPlaintextButListDoesNot(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	issued, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Token == "" || issued.WorkspaceID.Bytes != workspaceID.Bytes || issued.SourceID.Bytes != sourceID.Bytes {
		t.Fatal("issued credential does not carry its token and ownership")
	}
	if issued.RevokedAt != nil {
		t.Fatal("a new credential is already revoked")
	}
	list, err := service.List(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed credentials=%d want=1", len(list))
	}
	row := fake.stored(t, issued.ID)
	if !bytes.Equal(row.hash, tokenDigest(issued.Token)) {
		t.Fatal("stored digest does not match the issued token")
	}
	// Credential has no token or digest field at all, so metadata can never
	// leak them; assert the exposed values are only the fixed identity.
	listed := list[0]
	if listed.ID.Bytes != issued.ID.Bytes || listed.WorkspaceID.Bytes != workspaceID.Bytes || listed.SourceID.Bytes != sourceID.Bytes || listed.RevokedAt != nil {
		t.Fatal("listed metadata differs from the issued identity")
	}
}

func TestAuthenticateRejectsWithoutReason(t *testing.T) {
	service, _, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	issued, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(ctx, workspaceID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	unknownDigest := secretHash(tokenPrefix + "synthetic-unknown-token")
	cases := map[string]string{
		"missing prefix": "synthetic-token-without-prefix",
		"empty suffix":   tokenPrefix,
		"unknown":        tokenPrefix + "synthetic-unknown-token",
		"revoked":        revoked.Token,
	}
	for name, token := range cases {
		_, err := service.Authenticate(ctx, token)
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s case error=%v want ErrUnauthenticated", name, err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("%s case leaks ErrNotFound", name)
		}
		if strings.Contains(err.Error(), token) && len(token) > len(tokenPrefix) {
			t.Fatalf("%s case error leaks the token", name)
		}
		if strings.Contains(err.Error(), string(unknownDigest[:])) {
			t.Fatalf("%s case error leaks a digest", name)
		}
	}
	if _, err := service.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("active token error=%v want nil", err)
	}
}

func TestAuthenticateUsesStoredOwnership(t *testing.T) {
	fake := newFakeStore()
	workspaceID := testID(0x11)
	sourceID := testID(0x12)
	// The store row claims different ownership than the caller would expect; the
	// service must return the stored values, since Authenticate has no caller
	// workspace or source input at all.
	credential := Credential{ID: testID(0x13), WorkspaceID: workspaceID, SourceID: sourceID}
	token := tokenPrefix + "synthetic-fixed-ownership"
	fake.rows[credential.ID.Bytes] = storedCredential{credential: credential, hash: tokenDigest(token)}
	service := New(fake)
	got, err := service.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID.Bytes != credential.ID.Bytes || got.WorkspaceID.Bytes != workspaceID.Bytes || got.SourceID.Bytes != sourceID.Bytes {
		t.Fatal("authenticate did not return the stored ownership")
	}
}

func TestIssueRejectsForeignSource(t *testing.T) {
	service, fake, workspaceID, _ := newTestService(t)
	foreignSource := testID(0x21)
	_, err := service.Issue(context.Background(), workspaceID, foreignSource)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error=%v want ErrNotFound", err)
	}
	if fake.insertions() != 0 {
		t.Fatalf("inserted rows=%d want=0", fake.insertions())
	}
	if len(fake.digests) != 0 {
		t.Fatalf("stored digests=%d want=0", len(fake.digests))
	}
}

func TestRotateAndRevokeRejectForeignWorkspace(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	issued, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	foreignWorkspace := testID(0x31)
	if _, err := service.Rotate(ctx, foreignWorkspace, issued.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate error=%v want ErrNotFound", err)
	}
	if err := service.Revoke(ctx, foreignWorkspace, issued.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke error=%v want ErrNotFound", err)
	}
	target := fake.stored(t, issued.ID)
	if target.credential.RevokedAt != nil {
		t.Fatal("foreign workspace revoke changed the credential")
	}
	if _, err := service.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("credential stopped working after foreign attempts: %v", err)
	}
}

func TestRotateFromRevokedKeepsOldRow(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	old, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(ctx, workspaceID, old.ID); err != nil {
		t.Fatal(err)
	}
	before := fake.stored(t, old.ID)
	if before.credential.RevokedAt == nil {
		t.Fatal("setup did not revoke the old credential")
	}
	rotated, err := service.Rotate(ctx, workspaceID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID.Bytes == old.ID.Bytes {
		t.Fatal("rotation reused the old credential id")
	}
	if rotated.Token == "" || rotated.RevokedAt != nil {
		t.Fatal("rotation did not produce an active token")
	}
	if rotated.WorkspaceID.Bytes != workspaceID.Bytes || rotated.SourceID.Bytes != sourceID.Bytes {
		t.Fatal("rotation moved the credential to another workspace or source")
	}
	after := fake.stored(t, old.ID)
	if !after.credential.RevokedAt.Equal(*before.credential.RevokedAt) {
		t.Fatal("rotation changed the old revoked_at value")
	}
	if _, err := service.Authenticate(ctx, old.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old token error=%v want ErrUnauthenticated", err)
	}
	if _, err := service.Authenticate(ctx, rotated.Token); err != nil {
		t.Fatalf("new token error=%v want nil", err)
	}
	if fake.insertions() != 2 {
		t.Fatalf("inserted rows=%d want=2", fake.insertions())
	}
}

func TestLostIssueResponseRevokesByListedID(t *testing.T) {
	service, _, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	// The caller loses the issue response and only remembers the source.
	lost, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.List(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed credentials=%d want=1", len(list))
	}
	if err := service.Revoke(ctx, list[0].WorkspaceID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, lost.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("lost token error=%v want ErrUnauthenticated", err)
	}
	after, err := service.List(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].RevokedAt == nil {
		t.Fatal("list does not show the revoked state")
	}
}

func TestListEmptyForExistingSourceWithoutCredentials(t *testing.T) {
	service, _, workspaceID, sourceID := newTestService(t)
	list, err := service.List(context.Background(), workspaceID, sourceID)
	if err != nil {
		t.Fatalf("error=%v want nil for an existing source", err)
	}
	if list == nil {
		t.Fatal("list returned a nil slice instead of an empty one")
	}
	if len(list) != 0 {
		t.Fatalf("listed credentials=%d want=0", len(list))
	}
}

func TestListRejectsForeignOrMissingSource(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	if _, err := service.Issue(ctx, workspaceID, sourceID); err != nil {
		t.Fatal(err)
	}
	foreignWorkspace := testID(0x71)
	fake.addSource(foreignWorkspace, testID(0x72))
	for name, scope := range map[string]struct {
		workspaceID ID
		sourceID    ID
	}{
		"missing source":       {workspaceID, testID(0x73)},
		"foreign source":       {workspaceID, testID(0x72)},
		"foreign workspace":    {foreignWorkspace, sourceID},
		"foreign both missing": {foreignWorkspace, testID(0x74)},
	} {
		list, err := service.List(ctx, scope.workspaceID, scope.sourceID)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s list error=%v want ErrNotFound", name, err)
		}
		if list != nil {
			t.Fatalf("%s list returned %d credentials alongside ErrNotFound", name, len(list))
		}
	}
}

func TestListPropagatesSourceCheckError(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	failure := errors.New("synthetic source lookup failure")
	fake.errSource = failure
	list, err := service.List(context.Background(), workspaceID, sourceID)
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v want the store failure", err)
	}
	if list != nil {
		t.Fatal("failed list still returned credentials")
	}
}

func TestLostRotateResponseRevokesByListedID(t *testing.T) {
	service, _, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	// A is the credential the owner wants to keep; rotating it loses the
	// response, so the only trace of B is the extra row in the list.
	kept, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	lost, err := service.Rotate(ctx, workspaceID, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lost.ID.Bytes == kept.ID.Bytes {
		t.Fatal("rotation reused the kept credential id")
	}
	list, err := service.List(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed credentials=%d want=2", len(list))
	}
	var strays []Credential
	for _, credential := range list {
		if credential.ID.Bytes != kept.ID.Bytes {
			strays = append(strays, credential)
		}
		if credential.RevokedAt != nil {
			t.Fatal("an active credential was listed as revoked")
		}
	}
	if len(strays) != 1 {
		t.Fatalf("unexpected credentials beyond the kept one=%d want=1", len(strays))
	}
	if strays[0].ID.Bytes != lost.ID.Bytes {
		t.Fatal("the only extra credential is not the lost rotation result")
	}
	if err := service.Revoke(ctx, strays[0].WorkspaceID, strays[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, lost.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("lost rotation token error=%v want ErrUnauthenticated", err)
	}
	if _, err := service.Authenticate(ctx, kept.Token); err != nil {
		t.Fatalf("kept credential stopped working: %v", err)
	}
	after, err := service.List(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("listed credentials=%d want=2 after revoke", len(after))
	}
	revokedSeen, activeSeen := 0, 0
	for _, credential := range after {
		switch credential.ID.Bytes {
		case lost.ID.Bytes:
			if credential.RevokedAt == nil {
				t.Fatal("list does not show the revoked state of the lost rotation")
			}
			revokedSeen++
		case kept.ID.Bytes:
			if credential.RevokedAt != nil {
				t.Fatal("revoking the lost rotation also revoked the kept credential")
			}
			activeSeen++
		}
	}
	if revokedSeen != 1 || activeSeen != 1 {
		t.Fatalf("revoked=%d active=%d want 1/1", revokedSeen, activeSeen)
	}
}

func TestRepeatedRevokeKeepsFirstTime(t *testing.T) {
	service, fake, workspaceID, sourceID := newTestService(t)
	ctx := context.Background()
	issued, err := service.Issue(ctx, workspaceID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(ctx, workspaceID, issued.ID); err != nil {
		t.Fatal(err)
	}
	first := fake.stored(t, issued.ID).credential.RevokedAt
	if first == nil {
		t.Fatal("first revoke did not stamp revoked_at")
	}
	if err := service.Revoke(ctx, workspaceID, issued.ID); err != nil {
		t.Fatalf("repeated revoke error=%v want nil", err)
	}
	second := fake.stored(t, issued.ID).credential.RevokedAt
	if second == nil || !second.Equal(*first) {
		t.Fatal("repeated revoke changed revoked_at")
	}
	if fake.revokeCalls != 2 {
		t.Fatalf("store revoke calls=%d want=2", fake.revokeCalls)
	}
}

func TestStoreErrorsArePropagated(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("synthetic store failure")
	tests := []struct {
		name  string
		setup func(*fakeStore)
		call  func(*Service, ID, ID) error
	}{
		{
			name:  "authenticate",
			setup: func(f *fakeStore) { f.errByHash = failure },
			call: func(s *Service, workspaceID, sourceID ID) error {
				_, err := s.Authenticate(ctx, tokenPrefix+"synthetic-propagate")
				return err
			},
		},
		{
			// The source exists, so List passes the ownership check and the
			// store list failure below is the error that must surface.
			name:  "list",
			setup: func(f *fakeStore) { f.errList = failure },
			call: func(s *Service, workspaceID, sourceID ID) error {
				_, err := s.List(ctx, workspaceID, sourceID)
				return err
			},
		},
		{
			name:  "issue source check",
			setup: func(f *fakeStore) { f.errSource = failure },
			call: func(s *Service, workspaceID, sourceID ID) error {
				_, err := s.Issue(ctx, workspaceID, sourceID)
				return err
			},
		},
		{
			name:  "issue insert",
			setup: func(f *fakeStore) { f.errInsert = failure },
			call: func(s *Service, workspaceID, sourceID ID) error {
				_, err := s.Issue(ctx, workspaceID, sourceID)
				return err
			},
		},
		{
			name:  "rotate lookup",
			setup: func(f *fakeStore) { f.errByID = failure },
			call: func(s *Service, workspaceID, sourceID ID) error {
				_, err := s.Rotate(ctx, workspaceID, testID(0x41))
				return err
			},
		},
		{
			name:  "revoke",
			setup: func(f *fakeStore) { f.errRevoke = failure },
			call: func(s *Service, workspaceID, sourceID ID) error {
				return s.Revoke(ctx, workspaceID, testID(0x42))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeStore()
			workspaceID := testID(0x51)
			sourceID := testID(0x52)
			fake.addSource(workspaceID, sourceID)
			test.setup(fake)
			service := New(fake)
			err := test.call(service, workspaceID, sourceID)
			if !errors.Is(err, failure) {
				t.Fatalf("error=%v want the store failure", err)
			}
			if fake.insertions() != 0 {
				t.Fatal("failed call still inserted a row")
			}
		})
	}
}

func TestRotateRequiresExistingCredential(t *testing.T) {
	service, fake, workspaceID, _ := newTestService(t)
	_, err := service.Rotate(context.Background(), workspaceID, testID(0x61))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error=%v want ErrNotFound", err)
	}
	if fake.insertions() != 0 {
		t.Fatal("rotate of a missing credential inserted a row")
	}
}

var _ store = (*fakeStore)(nil)
