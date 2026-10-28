package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dundunHa/TokenPivot/internal/auth"
)

// Request body bounds. Authentication fields must parse unambiguously, so
// duplicated keys, unknown fields and trailing content are refused.
const maxAuthBodyBytes = 16 << 10

// authRequest is the only accepted register and login body: exactly two string
// fields, nothing else.
type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// userResponse is the public account shape. It never carries a password, a
// password hash or a session token.
type userResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// sessionResponse is the success body of register, login and session: the user
// and the session expiry in RFC 3339 form.
type sessionResponse struct {
	User      userResponse `json:"user"`
	ExpiresAt string       `json:"expires_at"`
}

// errorEnvelope is the shared error body. Messages never repeat a request value,
// a password or a session token.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleRegister creates an account and its first session, then returns 201 with
// the session cookie. An already owned email is 409, never a silent login.
func (a *Auth) handleRegister(w http.ResponseWriter, r *http.Request) {
	request, ok := readAuthRequest(w, r)
	if !ok {
		return
	}
	session, err := a.Service.Register(r.Context(), request.Email, request.Password, remoteIP(r))
	if err != nil {
		writeServiceError(r.Context(), w, err)
		return
	}
	a.setSessionCookie(w, session)
	writeJSON(w, http.StatusCreated, newSessionResponse(session))
}

// handleLogin verifies the password and issues a new session. Unknown email and
// wrong password share one 401 so login does not reveal whether an account exists.
func (a *Auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	request, ok := readAuthRequest(w, r)
	if !ok {
		return
	}
	session, err := a.Service.Login(r.Context(), request.Email, request.Password, remoteIP(r))
	if err != nil {
		writeServiceError(r.Context(), w, err)
		return
	}
	a.setSessionCookie(w, session)
	writeJSON(w, http.StatusOK, newSessionResponse(session))
}

// handleSession reports the renewed session. RequireSession already renewed the
// expiry in the database and wrote the matching cookie.
func (a *Auth) handleSession(w http.ResponseWriter, r *http.Request) {
	identity, ok := r.Context().Value(sessionContextKey{}).(sessionIdentity)
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		User:      userResponse{ID: identity.user.ID.String(), Email: identity.user.Email},
		ExpiresAt: identity.expiresAt.UTC().Format(time.RFC3339),
	})
}

// handleLogout revokes the current session if there is one and always clears the
// cookie, so repeated logout stays 204. A database failure is reported as a
// failure instead of claiming the session was revoked.
func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := sessionTokenFromCookie(r); token != "" {
		if err := a.Service.Logout(r.Context(), token); err != nil {
			writeServiceError(r.Context(), w, err)
			return
		}
	}
	clearSessionCookie(w, a.Origin)
	w.WriteHeader(http.StatusNoContent)
}

// readAuthRequest enforces the media type, the body size limit and the strict
// JSON rules, then returns the two decoded fields.
func readAuthRequest(w http.ResponseWriter, r *http.Request) (authRequest, bool) {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "content type must be application/json")
		return authRequest{}, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request body is too large")
			return authRequest{}, false
		}
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body could not be read")
		return authRequest{}, false
	}
	if !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be valid UTF-8")
		return authRequest{}, false
	}
	request, err := decodeAuthRequest(body)
	switch {
	case errors.Is(err, errAuthBodyShape):
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object without duplicate keys or trailing content")
		return authRequest{}, false
	case err != nil:
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object with exactly the email and password string fields")
		return authRequest{}, false
	}
	return request, true
}

// Strict body errors. They stay distinct so the response can name the documented
// cause, while neither ever repeats a request value.
var (
	// errAuthBodyShape reports a body that is not exactly one JSON object, has a
	// duplicated key in that object, or carries trailing content.
	errAuthBodyShape = errors.New("auth body: not one JSON object without duplicate keys or trailing content")
	// errAuthBodyFields reports an object whose fields are not exactly the two
	// known keys with string values.
	errAuthBodyFields = errors.New("auth body: fields must be exactly email and password strings")
)

// decodeAuthRequest parses the authentication body without relying on struct
// field matching. encoding/json matches object keys case-insensitively, so
// "EMAIL" would bind to Email and a body carrying both "email" and "EMAIL" would
// be ambiguous. This parser compares the decoded key exactly: only "email" and
// "password" are known fields, so a differently cased key is an unknown field,
// while an escaped spelling such as `"\u0065mail"` decodes to the same key and is
// accepted. The same decoded key twice is a duplicate.
func decodeAuthRequest(body []byte) (authRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return authRequest{}, errAuthBodyShape
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return authRequest{}, errAuthBodyShape
	}
	var request authRequest
	seen := make(map[string]struct{}, 2)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return authRequest{}, errAuthBodyShape
		}
		key, ok := keyToken.(string)
		if !ok {
			return authRequest{}, errAuthBodyShape
		}
		// The decoded key decides acceptance and is case-sensitive: an escaped
		// spelling of a known key is that same field, a differently cased key is
		// unknown, and the decoded key is also the duplicate-detection identity.
		switch key {
		case "email", "password":
		default:
			return authRequest{}, errAuthBodyFields
		}
		if _, duplicate := seen[key]; duplicate {
			return authRequest{}, errAuthBodyShape
		}
		seen[key] = struct{}{}
		valueToken, err := decoder.Token()
		if err != nil {
			return authRequest{}, errAuthBodyShape
		}
		value, ok := valueToken.(string)
		if !ok {
			return authRequest{}, errAuthBodyFields
		}
		if key == "email" {
			request.Email = value
		} else {
			request.Password = value
		}
	}
	if _, err := decoder.Token(); err != nil {
		return authRequest{}, errAuthBodyShape
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return authRequest{}, errAuthBodyShape
	}
	if len(seen) != 2 {
		return authRequest{}, errAuthBodyFields
	}
	return request, nil
}

// isJSONContentType accepts application/json with optional parameters such as a
// charset.
func isJSONContentType(value string) bool {
	mediaType, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "application/json")
}

// remoteIP is the only accepted client address: the transport RemoteAddr. Any
// forwarding header is ignored, so a proxy deployment needs its own trusted
// proxy design before IP limits mean real client addresses.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newSessionResponse converts a session into the public body.
func newSessionResponse(session auth.Session) sessionResponse {
	return sessionResponse{
		User:      userResponse{ID: session.User.ID.String(), Email: session.User.Email},
		ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// writeJSON writes one JSON body. A write failure means the peer disconnected
// and no request value is logged.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes the shared error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// writeServiceError maps a business error to its HTTP status and code without
// ever exposing a database message, a password or a token.
func writeServiceError(ctx context.Context, w http.ResponseWriter, err error) {
	var limited auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", retryAfterSeconds(limited.RetryAfter))
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many attempts; retry later")
	case errors.Is(err, auth.ErrInvalidArgument):
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request is not acceptable")
	case errors.Is(err, auth.ErrEmailUnavailable):
		writeError(w, http.StatusConflict, "EMAIL_UNAVAILABLE", "email is already registered")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
	case errors.Is(err, auth.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
	case errors.Is(err, auth.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "service is unavailable")
	default:
		slog.ErrorContext(ctx, "auth request failed", "stage", "auth", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

// retryAfterSeconds rounds a remaining window up to whole seconds, the unit of
// the Retry-After header, and never reports zero.
func retryAfterSeconds(remaining time.Duration) string {
	seconds := int64(remaining / time.Second)
	if remaining%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}
