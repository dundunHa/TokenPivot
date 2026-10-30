package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/config"
)

// Auth bundles the browser-session dependencies the HTTP layer needs: the
// account service and the single trusted origin. It is the only place that
// knows the cookie name and the session context key.
type Auth struct {
	Service *auth.Service
	Origin  config.Origin
	// DisableOriginCheck makes RequireOrigin pass every request through. It is set
	// only from TOKENPIVOT_DISABLE_ORIGIN_CHECK; session checks still apply.
	DisableOriginCheck bool
	// now reads the current time for the cookie Max-Age; nil means time.Now. It
	// is unexported and set only by tests, which pin the gap between the database
	// expiry and the response without sleeping.
	now func() time.Time
}

// cache-control for every authentication response: a cached session response
// would let a browser skip authentication, revocation or renewal.
const noStoreValue = "no-store"

// Auth routes. They are the browser-facing authentication API under /api/v1.
const (
	registerPath = "/api/v1/auth/register"
	loginPath    = "/api/v1/auth/login"
	sessionPath  = "/api/v1/auth/session"
	logoutPath   = "/api/v1/auth/logout"
)

// sessionCookieName is fixed: the cookie is host-scoped (no Domain), HttpOnly
// and SameSite=Lax, and Secure whenever the trusted origin is https.
const sessionCookieName = "tokenpivot_session"

// sessionContextKey is the private context key of the authenticated identity.
// Handlers read the user through UserFromContext only; they never parse a
// request body or path for a user ID.
type sessionContextKey struct{}

// sessionIdentity is what the session middleware puts in the request context:
// the authenticated user plus the expiry the database just assigned.
type sessionIdentity struct {
	user      auth.User
	expiresAt time.Time
}

// UserFromContext returns the authenticated browser user attached by the
// session middleware. It is false on unauthenticated requests, so callers must
// not treat a zero value as a valid identity.
func UserFromContext(ctx context.Context) (auth.User, bool) {
	identity, ok := ctx.Value(sessionContextKey{}).(sessionIdentity)
	return identity.user, ok
}

// routes registers the four authentication endpoints on mux. Every response,
// including Origin and session failures, is marked no-store by wrapping the
// whole path.
func (a *Auth) routes(mux *http.ServeMux) {
	if a == nil || a.Service == nil {
		return
	}
	mux.Handle("POST "+registerPath, a.noStore(a.RequireOrigin(http.HandlerFunc(a.handleRegister))))
	mux.Handle("POST "+loginPath, a.noStore(a.RequireOrigin(http.HandlerFunc(a.handleLogin))))
	mux.Handle("GET "+sessionPath, a.noStore(a.RequireSession(http.HandlerFunc(a.handleSession))))
	mux.Handle("POST "+logoutPath, a.noStore(a.RequireOrigin(http.HandlerFunc(a.handleLogout))))
}

// noStore marks a response as uncacheable before the wrapped handler runs.
func (a *Auth) noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", noStoreValue)
		next.ServeHTTP(w, r)
	})
}

// RequireOrigin rejects a browser write request unless it carries exactly one
// Origin value equal to the configured trusted origin. It never trusts the Host
// header, Forwarded or X-Forwarded-Host. It applies to browser write routes
// only; plugin Bearer endpoints keep their existing authentication. When
// DisableOriginCheck is set the check is skipped completely.
func (a *Auth) RequireOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.DisableOriginCheck && !config.OriginHeaderMatches(a.Origin, r.Header.Values("Origin")) {
			writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "request origin is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSession authenticates and renews the session cookie in one database
// statement, writes the renewed cookie and attaches the user to the request
// context. It is reusable for every protected browser API: missing, malformed,
// expired and revoked sessions all produce the same 401. A database failure
// produces 503 and never falls back to an anonymous identity.
func (a *Auth) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
			return
		}
		session, err := a.Service.Session(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				// The token is definitely not usable, so clearing it is safe here.
				clearSessionCookie(w, a.Origin)
				writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
				return
			}
			writeServiceError(r.Context(), w, err)
			return
		}
		a.setSessionCookie(w, session)
		ctx := context.WithValue(r.Context(), sessionContextKey{}, sessionIdentity{user: session.User, expiresAt: session.ExpiresAt})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// setSessionCookie writes the sliding session cookie: host-scoped, HttpOnly,
// SameSite=Lax, Secure on https, with the expiry the database returned and a
// Max-Age equal to the seconds left until that expiry, so a slow response or a
// clock difference can never claim a longer life than the stored session has.
func (a *Auth) setSessionCookie(w http.ResponseWriter, session auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session.Token,
		Path:     "/",
		Expires:  session.ExpiresAt,
		MaxAge:   sessionCookieMaxAge(session.ExpiresAt, a.clock()),
		HttpOnly: true,
		Secure:   a.Origin.Secure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// clock reads the clock the cookie Max-Age is measured against.
func (a *Auth) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// sessionCookieMaxAge converts the database expiry into whole seconds, rounded up
// so a sub-second response delay does not shorten a fresh 30-day window (2592000
// seconds). A non-positive remaining time means the expiry already passed, so the
// cookie is deleted: -1 serialises as Max-Age=0, and the past expiry the database
// returned is still sent as Expires.
func sessionCookieMaxAge(expiresAt, now time.Time) int {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return -1
	}
	seconds := remaining / time.Second
	if remaining%time.Second != 0 {
		seconds++
	}
	return int(seconds)
}

// clearSessionCookie removes the cookie with the same name, path and Secure
// attribute that issued it.
func clearSessionCookie(w http.ResponseWriter, origin config.Origin) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   origin.Secure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// sessionTokenFromCookie returns the cookie value, or the empty string.
func sessionTokenFromCookie(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}
