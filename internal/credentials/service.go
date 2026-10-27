// Package credentials issues, rotates and revokes ingest credentials and turns a
// plugin token into the workspace and source fixed when the credential was
// issued. Only the SHA-256 digest of a token is stored; the plaintext token is
// returned once and never logged.
package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ID is the PostgreSQL uuid identity shared by workspaces, sources and
// credentials.
type ID = pgtype.UUID

// tokenPrefix marks plugin ingest tokens. The random suffix length is owned by
// crypto/rand and is never hard coded by parsers.
const tokenPrefix = "tpk_"

var (
	// ErrUnauthenticated reports a token that is malformed, unknown or revoked.
	// The three causes are deliberately indistinguishable to callers.
	ErrUnauthenticated = errors.New("credentials: unauthenticated")
	// ErrNotFound reports a source or credential that does not belong to the
	// given workspace. It never distinguishes missing rows from foreign ones.
	ErrNotFound = errors.New("credentials: not found")
)

// Credential is the ingest identity: its fixed ownership plus revocation state.
// It never carries the token or its digest.
type Credential struct {
	ID          ID
	WorkspaceID ID
	SourceID    ID
	RevokedAt   *time.Time // nil means active
}

// IssuedCredential is the result of issuing or rotating a credential. Token is
// the plaintext token and is only available in this response.
type IssuedCredential struct {
	Credential
	Token string
}

// store is the narrow persistence surface the service needs. Every method
// scopes its work by workspace so foreign rows are unreachable, and none of
// them decides authentication, issuance or revocation policy.
type store interface {
	// List returns metadata for one workspace and source, revoked rows included.
	List(ctx context.Context, workspaceID, sourceID ID) ([]Credential, error)
	// BySecretHash returns the credential with this digest, if any.
	BySecretHash(ctx context.Context, secretHash []byte) (Credential, bool, error)
	// ByID returns the credential when it belongs to the workspace, if any.
	ByID(ctx context.Context, workspaceID, credentialID ID) (Credential, bool, error)
	// SourceExists reports whether the source belongs to the workspace.
	SourceExists(ctx context.Context, workspaceID, sourceID ID) (bool, error)
	// Insert stores one new credential row and returns its database identity.
	Insert(ctx context.Context, workspaceID, sourceID ID, secretHash []byte) (Credential, error)
	// Revoke stamps revoked_at once; false means no row matched.
	Revoke(ctx context.Context, workspaceID, credentialID ID) (bool, error)
}

// Service implements the credential use cases on top of a store.
type Service struct {
	store store
}

// New returns a Service backed by the given store.
func New(s store) *Service {
	return &Service{store: s}
}

// newToken returns a fresh plaintext token. crypto/rand.Text is used only when
// it produced randomness; an empty result is reported as an error, never
// retried silently or accepted.
func newToken() (string, error) {
	random := rand.Text()
	if random == "" {
		return "", errors.New("credentials: token generation failed")
	}
	return tokenPrefix + random, nil
}

// secretHash is the raw 32-byte SHA-256 digest of the full token.
func secretHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

// Authenticate resolves the fixed workspace and source of a plugin token.
// A malformed, unknown or revoked token is reported as ErrUnauthenticated
// without revealing which case applied. Ownership always comes from the stored
// row; database failures are returned as real errors and never fall back to an
// anonymous identity.
func (s *Service) Authenticate(ctx context.Context, token string) (Credential, error) {
	// Only the prefix is validated; the random suffix length stays owned by
	// crypto/rand and is never assumed here.
	if !strings.HasPrefix(token, tokenPrefix) {
		return Credential{}, ErrUnauthenticated
	}
	hash := secretHash(token)
	credential, found, err := s.store.BySecretHash(ctx, hash[:])
	if err != nil {
		return Credential{}, err
	}
	if !found || credential.RevokedAt != nil {
		return Credential{}, ErrUnauthenticated
	}
	return credential, nil
}

// List returns the metadata of every credential of the given workspace and
// source, including revoked ones. It never returns tokens or digests.
func (s *Service) List(ctx context.Context, workspaceID, sourceID ID) ([]Credential, error) {
	return s.store.List(ctx, workspaceID, sourceID)
}

// Issue creates a credential for an existing source of the workspace. A source
// outside the workspace yields ErrNotFound and inserts no row.
func (s *Service) Issue(ctx context.Context, workspaceID, sourceID ID) (IssuedCredential, error) {
	exists, err := s.store.SourceExists(ctx, workspaceID, sourceID)
	if err != nil {
		return IssuedCredential{}, err
	}
	if !exists {
		return IssuedCredential{}, ErrNotFound
	}
	return s.issue(ctx, workspaceID, sourceID)
}

// Rotate creates a new credential for the fixed workspace and source of an
// existing credential, active or revoked. The old row is left untouched: it is
// neither revived nor re-revoked, so both rows can be active at once.
func (s *Service) Rotate(ctx context.Context, workspaceID, credentialID ID) (IssuedCredential, error) {
	old, found, err := s.store.ByID(ctx, workspaceID, credentialID)
	if err != nil {
		return IssuedCredential{}, err
	}
	if !found {
		return IssuedCredential{}, ErrNotFound
	}
	return s.issue(ctx, old.WorkspaceID, old.SourceID)
}

// Revoke revokes one credential of the workspace. Revoking an already revoked
// credential succeeds and keeps the first revoked_at value; a credential
// outside the workspace yields ErrNotFound.
func (s *Service) Revoke(ctx context.Context, workspaceID, credentialID ID) error {
	revoked, err := s.store.Revoke(ctx, workspaceID, credentialID)
	if err != nil {
		return err
	}
	if !revoked {
		return ErrNotFound
	}
	return nil
}

// issue generates a token and stores only its digest.
func (s *Service) issue(ctx context.Context, workspaceID, sourceID ID) (IssuedCredential, error) {
	token, err := newToken()
	if err != nil {
		return IssuedCredential{}, err
	}
	hash := secretHash(token)
	credential, err := s.store.Insert(ctx, workspaceID, sourceID, hash[:])
	if err != nil {
		return IssuedCredential{}, err
	}
	return IssuedCredential{Credential: credential, Token: token}, nil
}
