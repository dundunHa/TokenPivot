// Package auth owns browser accounts, password verification and the opaque
// sessions behind the tokenpivot_session cookie. Only an Argon2id encoding of a
// password and a SHA-256 digest of a session token are ever stored; plaintext
// values exist only in the issuing response and are never logged.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Session token format: a fixed prefix plus crypto/rand.Text(), which always
// yields at least 128 bits of randomness. The random part length stays owned by
// crypto/rand and is never hard coded here.
const sessionTokenPrefix = "tps_"

// SessionLifetime is the exact sliding session length: every valid request moves
// the expiry to now plus this duration, with no absolute cap.
const SessionLifetime = 30 * 24 * time.Hour

// Accepted account input bounds.
const (
	emailMinLength  = 3
	emailMaxLength  = 254
	passwordMinRune = 8
	passwordMaxRune = 64
	passwordMaxByte = 256
)

var (
	// ErrInvalidArgument reports an account input that fails format validation.
	// The message never repeats the rejected value.
	ErrInvalidArgument = errors.New("auth: invalid argument")
	// ErrEmailUnavailable reports an email already owned by another account.
	ErrEmailUnavailable = errors.New("auth: email unavailable")
	// ErrInvalidCredentials reports a failed login. Unknown email and wrong
	// password are deliberately indistinguishable.
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	// ErrUnauthenticated reports a session token that is malformed, unknown,
	// expired or revoked. The four causes are deliberately indistinguishable.
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	// ErrUnavailable reports a dependency failure, so a broken database is never
	// reported as a credential problem.
	ErrUnavailable = errors.New("auth: dependency unavailable")
)

// RateLimitedError reports an exhausted fixed window. RetryAfter is the time
// left in that window.
type RateLimitedError struct{ RetryAfter time.Duration }

// Error implements error with fixed text that carries no account data.
func (e RateLimitedError) Error() string { return "auth: rate limited" }

// ID is the PostgreSQL uuid identity of a user.
type ID [16]byte

// String formats the UUID in canonical RFC 4122 form.
func (id ID) String() string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}

// User is the authenticated browser identity placed in the request context. It
// never carries a password, a session token or any workspace authorization.
type User struct {
	ID    ID
	Email string
}

// Credential is one stored account with its password encoding. It is only used
// to verify a login attempt and never leaves this package.
type Credential struct {
	User         User
	PasswordHash string
}

// Session is an issued or renewed browser session. Token is the plaintext token
// and exists only in the response that issues or refreshes it.
type Session struct {
	Token     string
	User      User
	ExpiresAt time.Time
}

// IssuedSession is the stored-session result a store returns after writing a row.
type IssuedSession struct {
	User      User
	ExpiresAt time.Time
}

// Store is the narrow persistence surface the service needs. It owns SQL only:
// no method decides validation, hashing, rate limiting or session policy, and
// none returns a plaintext token.
type Store interface {
	// UserCredential returns the account for a normalized email together with
	// its stored password encoding; found=false means no such account.
	UserCredential(ctx context.Context, email string) (Credential, bool, error)
	// CreateUserWithSession inserts the account and its first session as one
	// atomic unit and returns the expiry assigned by the database.
	CreateUserWithSession(ctx context.Context, email, passwordHash string, tokenHash []byte) (IssuedSession, error)
	// CreateSession stores one new session for an existing account.
	CreateSession(ctx context.Context, userID ID, tokenHash []byte) (time.Time, error)
	// AuthenticateSession renews one valid session and returns its user and new
	// expiry; found=false means unknown, expired or revoked.
	AuthenticateSession(ctx context.Context, tokenHash []byte) (IssuedSession, bool, error)
	// RevokeSession stamps revoked_at once for the digest. An unknown digest is
	// not an error, so repeated logout is idempotent.
	RevokeSession(ctx context.Context, tokenHash []byte) error
}

// TxRunner runs a function inside one database transaction. The store handed to
// the function is bound to that transaction, so every write it performs commits
// or rolls back as a unit. Future callers that must create a workspace in the
// same unit as registration add their writes inside this function.
type TxRunner interface {
	InTx(ctx context.Context, fn func(context.Context, Store) error) error
}

// Service implements the browser account use cases on top of a store.
type Service struct {
	store   Store
	tx      TxRunner
	limiter *Limiter
}

// New returns a Service. Store reads outside a transaction use store; Register
// uses tx so the account and its first session are one atomic unit.
func New(store Store, tx TxRunner, limiter *Limiter) *Service {
	return &Service{store: store, tx: tx, limiter: limiter}
}

// Register validates the account input, reserves one rate-limit slot in both
// windows, hashes the password and creates the account and its first session in
// one transaction. The new session is returned so the caller can set a cookie.
func (s *Service) Register(ctx context.Context, email, password, remoteIP string) (Session, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return Session{}, err
	}
	if err := validatePassword(password); err != nil {
		return Session{}, err
	}
	if retryAfter, ok := s.limiter.Take(normalized, remoteIP); !ok {
		return Session{}, RateLimitedError{RetryAfter: retryAfter}
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return Session{}, err
	}
	token := newSessionToken()
	var issued IssuedSession
	err = s.tx.InTx(ctx, func(ctx context.Context, tx Store) error {
		created, err := tx.CreateUserWithSession(ctx, normalized, passwordHash, sessionTokenHash(token))
		if err != nil {
			return err
		}
		issued = created
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, User: issued.User, ExpiresAt: issued.ExpiresAt}, nil
}

// Login verifies the password and issues a fresh session. An unknown email and
// a wrong password both run exactly one Argon2id derivation and return
// ErrInvalidCredentials, so login does not reveal whether an account exists.
func (s *Service) Login(ctx context.Context, email, password, remoteIP string) (Session, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return Session{}, err
	}
	if err := validatePassword(password); err != nil {
		return Session{}, err
	}
	if retryAfter, ok := s.limiter.Take(normalized, remoteIP); !ok {
		return Session{}, RateLimitedError{RetryAfter: retryAfter}
	}
	credential, found, err := s.store.UserCredential(ctx, normalized)
	if err != nil {
		return Session{}, err
	}
	if !found {
		// Pay the same derivation cost as a real account, then fail identically.
		if _, err := VerifyPassword(dummyPasswordHash(), password); err != nil {
			return Session{}, fmt.Errorf("auth: dummy password verification: %w", err)
		}
		return Session{}, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(credential.PasswordHash, password)
	if err != nil {
		return Session{}, fmt.Errorf("auth: stored password verification: %w", err)
	}
	if !ok {
		return Session{}, ErrInvalidCredentials
	}
	token := newSessionToken()
	expiresAt, err := s.store.CreateSession(ctx, credential.User.ID, sessionTokenHash(token))
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, User: credential.User, ExpiresAt: expiresAt}, nil
}

// Session authenticates and renews one session in a single database statement,
// so the 30-day window restarts on every valid request with no refresh threshold
// and no separate last-seen column. A malformed token is rejected before any SQL.
func (s *Service) Session(ctx context.Context, token string) (Session, error) {
	hash, err := tokenDigest(token)
	if err != nil {
		return Session{}, err
	}
	issued, found, err := s.store.AuthenticateSession(ctx, hash)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, ErrUnauthenticated
	}
	return Session{Token: token, User: issued.User, ExpiresAt: issued.ExpiresAt}, nil
}

// Logout revokes the session of this token. A malformed or unknown token is not
// an error: the caller still clears the cookie, and repeated logout keeps the
// first revoked_at value. A database failure is returned so the caller does not
// claim the session was revoked.
func (s *Service) Logout(ctx context.Context, token string) error {
	hash, err := tokenDigest(token)
	if err != nil {
		return nil
	}
	return s.store.RevokeSession(ctx, hash)
}

// normalizeEmail returns the single stored account form of an email: trimmed,
// lowercased and then validated as a product account name. It does not claim to
// implement the full RFC email grammar.
func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < emailMinLength || len(email) > emailMaxLength {
		return "", fmt.Errorf("%w: email length", ErrInvalidArgument)
	}
	for _, r := range email {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", fmt.Errorf("%w: email contains control or space characters", ErrInvalidArgument)
		}
	}
	at := strings.IndexByte(email, '@')
	if at < 0 || at == 0 || at == len(email)-1 || strings.Count(email, "@") != 1 {
		return "", fmt.Errorf("%w: email must contain exactly one @ between non-empty sides", ErrInvalidArgument)
	}
	return email, nil
}

// validatePassword checks the accepted length range only. The password is never
// trimmed or normalized: the exact user bytes are hashed.
func validatePassword(password string) error {
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: password is not valid UTF-8", ErrInvalidArgument)
	}
	if len(password) > passwordMaxByte {
		return fmt.Errorf("%w: password is too long", ErrInvalidArgument)
	}
	if n := utf8.RuneCountInString(password); n < passwordMinRune || n > passwordMaxRune {
		return fmt.Errorf("%w: password must be 8 to 64 characters", ErrInvalidArgument)
	}
	return nil
}

// newSessionToken returns a fresh opaque token for one session. A new token is
// generated for every register, login and renewal, so a supplied cookie can
// never fixate a session.
func newSessionToken() string {
	return sessionTokenPrefix + rand.Text()
}

// tokenDigest returns the raw 32-byte SHA-256 digest of the full token. The
// plugin tpk_ prefix is deliberately not accepted.
func tokenDigest(token string) ([]byte, error) {
	if !strings.HasPrefix(token, sessionTokenPrefix) {
		return nil, ErrUnauthenticated
	}
	return sessionTokenHash(token), nil
}

// sessionTokenHash returns the raw 32-byte SHA-256 digest of a token already
// known to carry the session prefix, such as one built by newSessionToken.
func sessionTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
