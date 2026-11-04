package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
)

// Feishu route and cookie names. The state and bind cookies share one path so
// they never travel with an unrelated API request, and neither is readable by
// page script.
const (
	feishuAuthorizePath = "/api/v1/auth/feishu/authorize"
	feishuCallbackPath  = "/api/v1/auth/feishu/callback"
	feishuBindPath      = "/api/v1/auth/feishu/bind"
	// feishuCookiePath bounds both short-lived cookies to the Feishu endpoints.
	feishuCookiePath      = "/api/v1/auth/feishu"
	feishuStateCookieName = "tokenpivot_feishu_state"
	feishuBindCookieName  = "tokenpivot_feishu_bind"
	// loginPagePath is the frontend route every callback result returns to.
	loginPagePath = "/login"
)

// feishuAuthorizeResponse is the body of the authorize query. A nil
// AuthorizeURL serialises as null, which is what a disabled instance reports.
type feishuAuthorizeResponse struct {
	Enabled          bool    `json:"enabled"`
	AuthorizeURL     *string `json:"authorize_url"`
	ExpiresInSeconds int     `json:"expires_in_seconds"`
}

// handleFeishuAuthorize returns a fresh authorization address and stores only
// the digest of its state in a short-lived HttpOnly cookie. A disabled instance
// still answers 200 with enabled=false: "no Feishu application configured" is a
// deployment fact the login page renders, not an error.
func (a *Auth) handleFeishuAuthorize(w http.ResponseWriter, r *http.Request) {
	if !a.Service.FeishuEnabled() {
		writeJSON(w, http.StatusOK, feishuAuthorizeResponse{})
		return
	}
	state := rand.Text()
	http.SetCookie(w, a.feishuCookie(feishuStateCookieName, auth.FeishuStateDigest(state), int(auth.FeishuStateLifetime.Seconds())))
	authorizeURL := a.Service.FeishuAuthorizeURL(state)
	writeJSON(w, http.StatusOK, feishuAuthorizeResponse{
		Enabled:          true,
		AuthorizeURL:     &authorizeURL,
		ExpiresInSeconds: int(auth.FeishuStateLifetime.Seconds()),
	})
}

// handleFeishuCallback completes one authorization. It is a browser top-level
// navigation, so every outcome is a 302 with an empty body: the state cookie is
// cleared first, then the result either signs the browser in, asks it to confirm
// the existing account password, or names the failure in the query string.
func (a *Auth) handleFeishuCallback(w http.ResponseWriter, r *http.Request) {
	a.clearFeishuCookie(w, feishuStateCookieName)
	if !validFeishuState(r) {
		redirectFound(w, loginPagePath+"?error=FEISHU_STATE_INVALID")
		return
	}
	result, err := a.Service.FeishuLogin(r.Context(), r.URL.Query().Get("code"), remoteIP(r))
	if err != nil {
		a.redirectFeishuError(w, r, err)
		return
	}
	if result.BindToken != "" {
		http.SetCookie(w, a.feishuCookie(feishuBindCookieName, result.BindToken, int(auth.FeishuBindLifetime.Seconds())))
		redirectFound(w, loginPagePath+"?feishu_bind=1&email="+url.QueryEscape(result.Email))
		return
	}
	if result.Session == nil {
		redirectFound(w, loginPagePath+"?error=FEISHU_AUTH_FAILED")
		return
	}
	a.setSessionCookie(w, *result.Session)
	redirectFound(w, "/")
}

// handleFeishuBind confirms a same-email binding with the existing account
// password. The email and the Feishu identity come only from the pending cookie
// context; the body may carry nothing but the password, so a caller cannot point
// the bind at another account.
func (a *Auth) handleFeishuBind(w http.ResponseWriter, r *http.Request) {
	if !a.Service.FeishuEnabled() {
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "service is unavailable")
		return
	}
	token := feishuBindTokenFromCookie(r)
	if token == "" {
		a.clearFeishuCookie(w, feishuBindCookieName)
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return
	}
	password, ok := readFeishuBindRequest(w, r)
	if !ok {
		return
	}
	session, err := a.Service.FeishuBind(r.Context(), token, password, remoteIP(r))
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			// The pending credential is unusable, so the cookie must go too.
			a.clearFeishuCookie(w, feishuBindCookieName)
		}
		writeServiceError(r.Context(), w, err)
		return
	}
	a.clearFeishuCookie(w, feishuBindCookieName)
	a.setSessionCookie(w, session)
	writeJSON(w, http.StatusOK, newSessionResponse(session))
}

// feishuBindRequest is the only accepted bind body: exactly one string field.
type feishuBindRequest struct {
	Password string `json:"password"`
}

// readFeishuBindRequest enforces the shared body rules, then the stricter bind
// shape: one object with exactly the password string.
func readFeishuBindRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, ok := readJSONBody(w, r)
	if !ok {
		return "", false
	}
	request, err := decodeFeishuBindRequest(body)
	switch {
	case errors.Is(err, errAuthBodyShape):
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object without duplicate keys or trailing content")
		return "", false
	case err != nil:
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object with exactly the password string field")
		return "", false
	}
	return request.Password, true
}

// decodeFeishuBindRequest parses the bind body with the same decoded-key
// comparison as decodeAuthRequest: only "password" is known, a differently cased
// key is unknown, a repeated decoded key is a duplicate and trailing content is
// refused.
func decodeFeishuBindRequest(body []byte) (feishuBindRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return feishuBindRequest{}, errAuthBodyShape
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return feishuBindRequest{}, errAuthBodyShape
	}
	var request feishuBindRequest
	seen := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return feishuBindRequest{}, errAuthBodyShape
		}
		key, ok := keyToken.(string)
		if !ok {
			return feishuBindRequest{}, errAuthBodyShape
		}
		if key != "password" {
			return feishuBindRequest{}, errAuthBodyFields
		}
		if seen {
			return feishuBindRequest{}, errAuthBodyShape
		}
		seen = true
		valueToken, err := decoder.Token()
		if err != nil {
			return feishuBindRequest{}, errAuthBodyShape
		}
		value, ok := valueToken.(string)
		if !ok {
			return feishuBindRequest{}, errAuthBodyFields
		}
		request.Password = value
	}
	if _, err := decoder.Token(); err != nil {
		return feishuBindRequest{}, errAuthBodyShape
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return feishuBindRequest{}, errAuthBodyShape
	}
	if !seen {
		return feishuBindRequest{}, errAuthBodyFields
	}
	return request, nil
}

// validFeishuState proves the callback belongs to an authorize response this
// browser received. Both the state and the code must be present, and the stored
// digest is compared in constant time so a wrong value leaks no timing signal.
func validFeishuState(r *http.Request) bool {
	query := r.URL.Query()
	state := query.Get("state")
	if state == "" || query.Get("code") == "" {
		return false
	}
	cookie, err := r.Cookie(feishuStateCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(auth.FeishuStateDigest(state))) == 1
}

// redirectFeishuError maps a callback failure to its documented redirect code.
// An unexpected failure is logged with a fixed stage and still redirects: the
// callback is a navigation, so a JSON error body would strand the browser.
func (a *Auth) redirectFeishuError(w http.ResponseWriter, r *http.Request, err error) {
	var limited auth.RateLimitedError
	code := "FEISHU_AUTH_FAILED"
	switch {
	case errors.As(err, &limited):
		code = "RATE_LIMITED"
	case errors.Is(err, auth.ErrFeishuEmailRequired):
		code = "FEISHU_EMAIL_REQUIRED"
	case errors.Is(err, auth.ErrRegistrationClosed):
		code = "REGISTRATION_CLOSED"
	case errors.Is(err, auth.ErrFeishuAuthFailed), errors.Is(err, auth.ErrEmailUnavailable):
		// A refused exchange or a concurrent first-account creation is an expected
		// failure, not an internal fault.
	default:
		slog.ErrorContext(r.Context(), "feishu callback failed", "stage", "feishu", "err", err)
	}
	redirectFound(w, loginPagePath+"?error="+code)
}

// redirectFound writes an empty 302. http.Redirect would add an HTML body, and
// the callback contract is an empty navigation response.
func redirectFound(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

// feishuCookie builds one short-lived Feishu cookie. It is host-scoped,
// HttpOnly, SameSite=Lax and Secure whenever the trusted origin is https, the
// same rules as the session cookie.
func (a *Auth) feishuCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     feishuCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.Origin.Secure(),
		SameSite: http.SameSiteLaxMode,
	}
}

// clearFeishuCookie removes one Feishu cookie with the same path and Secure
// attribute that issued it.
func (a *Auth) clearFeishuCookie(w http.ResponseWriter, name string) {
	cookie := a.feishuCookie(name, "", -1)
	cookie.Expires = time.Unix(0, 0).UTC()
	http.SetCookie(w, cookie)
}

// feishuBindTokenFromCookie returns the pending-binding token, or empty.
func feishuBindTokenFromCookie(r *http.Request) string {
	cookie, err := r.Cookie(feishuBindCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}
