package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SQL statements are spelled out once. Expiry is always computed by the
// database, so application and database clock differences never decide session
// validity.
const (
	userCredentialQuery      = `SELECT id, email, password_hash FROM users WHERE email = $1`
	createUserQuery          = `INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, email`
	createSessionQuery       = `INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days') RETURNING expires_at`
	authenticateSessionQuery = `UPDATE sessions
SET expires_at = now() + interval '30 days'
FROM users
WHERE sessions.token_hash = $1
  AND users.id = sessions.user_id
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > now()
RETURNING users.id, users.email, sessions.expires_at`
	revokeSessionQuery = `UPDATE sessions SET revoked_at = COALESCE(revoked_at, now()) WHERE token_hash = $1`
	// lockRegistrationQuery blocks until this transaction holds the exclusive
	// transactional registration lock. PostgreSQL releases it when the
	// transaction commits or rolls back, so it needs no unlock statement.
	lockRegistrationQuery = `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`
	hasUsersQuery         = `SELECT EXISTS (SELECT 1 FROM users)`
)

// The registration lock key is two fixed 32-bit integers, matching PostgreSQL's
// two-integer advisory lock space, and never a string hash that could collide.
// It is a package constant rather than configuration: two deployments sharing a
// database must contend on the same key. The key is taken; a future version that
// replaces it must keep older versions from both accepting an empty database.
const (
	registrationLockClass  int32 = 17017
	registrationLockObject int32 = 1
)

// querier is the subset shared by a pool and an open transaction. It keeps every
// statement usable both inside and outside the registration transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// sqlStore implements Store on one querier. Errors leaving it carry fixed text
// only; SQL, driver messages, tokens, hashes and emails are never included.
type sqlStore struct{ q querier }

// Postgres is the production store and transaction runner. The embedded store
// answers reads on the pool; InTx hands a transaction-bound store to the caller.
type Postgres struct {
	pool *pgxpool.Pool
	*sqlStore
}

// NewPostgres returns a store on the caller-owned pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool, sqlStore: &sqlStore{q: pool}}
}

// InTx runs fn inside one explicit READ COMMITTED transaction. The store passed
// to fn is bound to that transaction, so the account, personal workspace, owner
// membership and first session it writes commit or roll back as one unit. The
// isolation level is spelled out because the closed-registration path relies on
// READ COMMITTED: the statements after a lock wait take a fresh snapshot and see
// the user that the previous lock holder committed. A nil function result
// commits; any error rolls back and is returned as the caller supplied it.
func (p *Postgres) InTx(ctx context.Context, fn func(context.Context, RegistrationStore) error) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return safeStoreError("auth begin", err)
	}
	defer func() {
		// Rollback after a successful commit reports a closed transaction, which
		// is expected and deliberately ignored.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	if err := fn(ctx, &registrationTxStore{sqlStore: &sqlStore{q: tx}, workspaces: workspaces.NewTx(tx)}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return safeStoreError("auth commit", err)
	}
	return nil
}

// registrationTxStore is the transaction-bound registration store. It answers the
// account and session statements through the shared sqlStore on this transaction
// and reaches personal workspace creation through the workspaces transaction
// adapter, so the order and the SQL of that step stay owned by internal/workspaces.
type registrationTxStore struct {
	*sqlStore
	workspaces *workspaces.Tx
}

// CreateUser inserts the account and returns the identity the database assigned.
func (s *registrationTxStore) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	var user User
	var id ID
	if err := s.q.QueryRow(ctx, createUserQuery, email, passwordHash).Scan(&id, &user.Email); err != nil {
		return User{}, mapUserConflict(err)
	}
	user.ID = id
	return user, nil
}

// CreatePersonal creates the user's initial personal workspace and owner
// membership in this same transaction. The returned workspace is not part of the
// registration response, so it is dropped here instead of being exposed.
func (s *registrationTxStore) CreatePersonal(ctx context.Context, userID ID) error {
	if _, err := s.workspaces.CreatePersonal(ctx, workspaces.ID{Bytes: userID, Valid: true}); err != nil {
		return err
	}
	return nil
}

// LockRegistration takes the exclusive transactional registration lock and holds
// it until this transaction ends.
func (s *registrationTxStore) LockRegistration(ctx context.Context) error {
	if _, err := s.q.Exec(ctx, lockRegistrationQuery, registrationLockClass, registrationLockObject); err != nil {
		return safeStoreError("auth registration lock", err)
	}
	return nil
}

// HasUsers reports whether any account exists. On READ COMMITTED every statement
// takes a fresh snapshot, so after waiting on the registration lock this query
// sees the user committed by the previous lock holder.
func (s *registrationTxStore) HasUsers(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.q.QueryRow(ctx, hasUsersQuery).Scan(&exists); err != nil {
		return false, safeStoreError("auth registration state", err)
	}
	return exists, nil
}

// UserCredential returns the account of a normalized email. A miss is not an
// error, so the service can run the dummy verification instead.
func (s *sqlStore) UserCredential(ctx context.Context, email string) (Credential, bool, error) {
	var credential Credential
	var id ID
	err := s.q.QueryRow(ctx, userCredentialQuery, email).Scan(&id, &credential.User.Email, &credential.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, false, nil
	}
	if err != nil {
		return Credential{}, false, safeStoreError("auth account lookup", err)
	}
	credential.User.ID = id
	return credential, true, nil
}

// CreateSession issues one additional session for an existing account.
func (s *sqlStore) CreateSession(ctx context.Context, userID ID, tokenHash []byte) (time.Time, error) {
	return s.insertSession(ctx, userID, tokenHash)
}

func (s *sqlStore) insertSession(ctx context.Context, userID ID, tokenHash []byte) (time.Time, error) {
	var expiresAt time.Time
	if err := s.q.QueryRow(ctx, createSessionQuery, userID, tokenHash).Scan(&expiresAt); err != nil {
		return time.Time{}, safeStoreError("auth session insert", err)
	}
	return expiresAt, nil
}

// AuthenticateSession renews one valid session and returns its user and the new
// expiry in a single statement. Zero rows means unknown, expired or revoked and
// is reported as found=false; the three causes stay indistinguishable.
func (s *sqlStore) AuthenticateSession(ctx context.Context, tokenHash []byte) (IssuedSession, bool, error) {
	var issued IssuedSession
	var id ID
	err := s.q.QueryRow(ctx, authenticateSessionQuery, tokenHash).Scan(&id, &issued.User.Email, &issued.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssuedSession{}, false, nil
	}
	if err != nil {
		return IssuedSession{}, false, safeStoreError("auth session authenticate", err)
	}
	issued.User.ID = id
	return issued, true, nil
}

// RevokeSession stamps revoked_at once. An unknown digest affects zero rows and
// is not an error, so repeated logout keeps the first revocation time.
func (s *sqlStore) RevokeSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.q.Exec(ctx, revokeSessionQuery, tokenHash); err != nil {
		return safeStoreError("auth session revoke", err)
	}
	return nil
}

// mapUserConflict translates only the email unique index conflict into the
// business error; every other database failure keeps its safe generic form.
func mapUserConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uk_users_email" {
		return ErrEmailUnavailable
	}
	return safeStoreError("auth registration", err)
}

// safeStoreError keeps the fixed stage text of database.SafeError and adds
// ErrUnavailable when the database itself could not be used. Without this
// distinction an unreachable or restarting database would look like an internal
// fault instead of the documented 503 UNAVAILABLE.
func safeStoreError(stage string, err error) error {
	safe := database.SafeError(stage, err)
	if isDatabaseUnavailable(err) {
		return errors.Join(ErrUnavailable, safe)
	}
	return safe
}

// isDatabaseUnavailable recognizes dependency failures, not statement faults:
// failed connection attempts, a connection the driver already closed and
// SQLSTATE class 08 (connection exception). A client disconnect is not a
// dependency failure, so a cancelled context stays an internal failure.
func isDatabaseUnavailable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, pgconn.ErrConnClosed) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "08")
}
