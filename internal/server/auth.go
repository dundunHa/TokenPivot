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
	setSessionCookie(w, a.Origin, session)
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
	setSessionCookie(w, a.Origin, session)
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
	if err := rejectDuplicateKeys(body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object without duplicate keys or trailing content")
		return authRequest{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request authRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object with email and password")
		return authRequest{}, false
	}
	return request, true
}

// rejectDuplicateKeys walks the whole JSON value once and fails on a repeated
// key in the same object, so a proxy and this service can never read different
// credentials from one body. It also rejects trailing content.
func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("auth body: trailing content")
	}
	return nil
}

// scanJSONValue consumes exactly one JSON value and checks object keys.
func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("auth body: object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("auth body: duplicate object key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return errors.New("auth body: unexpected delimiter")
	}
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
