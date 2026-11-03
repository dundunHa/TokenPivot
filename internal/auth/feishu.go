package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Feishu (Lark) sign-in constants. The lifetimes match the documented cookie
// Max-Age values: a state is usable for three minutes and a pending binding for
// ten, both far shorter than a session.
const (
	// FeishuStateLifetime is how long one authorize URL and its state stay valid.
	FeishuStateLifetime = 180 * time.Second
	// FeishuBindLifetime is how long a same-email binding may wait for the
	// account password confirmation.
	FeishuBindLifetime = 10 * time.Minute
	// feishuBindTokenPrefix marks the pending-binding token so a value of another
	// kind can never be mistaken for it.
	feishuBindTokenPrefix = "tpb_"
	// feishuCallbackRateKey namespaces the callback attempts of one address in
	// the shared login window, so a callback and a login of the same address do
	// not silently share a counter that was never meant for them.
	feishuCallbackRateKey = "feishu-cb:"
	// defaultFeishuBaseURL is the Feishu open platform origin; a deployment only
	// overrides it to reach a stub service.
	defaultFeishuBaseURL = "https://open.feishu.cn"
	// feishuHTTPTimeout bounds one outbound Feishu request. The token exchange
	// runs inside a browser callback, so it must not hang that navigation.
	feishuHTTPTimeout = 10 * time.Second
	// maxFeishuResponseBytes bounds one Feishu response body.
	maxFeishuResponseBytes = 1 << 20
)

// Feishu open platform paths.
const (
	feishuAppTokenPath        = "/open-apis/auth/v3/app_access_token/internal"
	feishuOIDCAccessTokenPath = "/open-apis/authen/v1/oidc/access_token"
	feishuAccessTokenPath     = "/open-apis/authen/v1/access_token"
	feishuUserInfoPath        = "/open-apis/authen/v1/user_info"
	feishuAuthorizePath       = "/open-apis/authen/v1/authorize"
)

// FeishuConfig is the immutable startup configuration of Feishu sign-in. It is
// installed once by EnableFeishu; values read from the environment never change
// afterwards. AppSecret is used only in requests to the open platform.
type FeishuConfig struct {
	// AppID and AppSecret are the application credentials. Both are required for
	// Enabled to report true.
	AppID     string
	AppSecret string
	// RedirectURI is the absolute callback address registered in the Feishu
	// application. It is derived from the trusted Web origin when not configured.
	RedirectURI string
	// BaseURL is the open platform origin, defaulting to https://open.feishu.cn.
	BaseURL string
	// AutoProvision allows a Feishu-verified account to be created while ordinary
	// email registration is closed.
	AutoProvision bool
	// HTTPClient overrides the outbound client. It exists so tests can point at a
	// local stub service; nil means a bounded default client.
	HTTPClient *http.Client
}

// Enabled reports whether the instance may offer Feishu sign-in: both
// application credentials must be present, and neither one alone enables it.
func (c FeishuConfig) Enabled() bool { return c.AppID != "" && c.AppSecret != "" }

// withDefaults fills the two derived fields so callers may leave them empty.
func (c FeishuConfig) withDefaults() FeishuConfig {
	if c.BaseURL == "" {
		c.BaseURL = defaultFeishuBaseURL
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c
}

// feishuClient is the minimal outbound HTTP surface of the token exchange.
type feishuClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// EnableFeishu installs the Feishu configuration. It is called once at startup;
// a zero value keeps FeishuEnabled false and the callback refusing every attempt.
func (s *Service) EnableFeishu(cfg FeishuConfig) {
	s.feishu = cfg.withDefaults()
	if cfg.HTTPClient != nil {
		s.feishuClient = cfg.HTTPClient
		return
	}
	s.feishuClient = &http.Client{Timeout: feishuHTTPTimeout}
}

// FeishuEnabled reports whether this service may offer Feishu sign-in.
func (s *Service) FeishuEnabled() bool { return s.feishu.Enabled() }

// FeishuAuthorizeURL builds the browser navigation target that starts the
// authorization. Every value is query-escaped, so a redirect URI carrying its own
// query string still arrives intact at the open platform.
func (s *Service) FeishuAuthorizeURL(state string) string {
	query := url.Values{}
	query.Set("app_id", s.feishu.AppID)
	query.Set("redirect_uri", s.feishu.RedirectURI)
	query.Set("state", state)
	return s.feishu.BaseURL + feishuAuthorizePath + "?" + query.Encode()
}

// FeishuStateDigest returns the lowercase hex SHA-256 digest of a state value.
// Only the digest is stored in the state cookie, so page script cannot reuse a
// state it can read.
func FeishuStateDigest(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// FeishuIdentity is one verified Feishu profile. Email is normalized before it
// is stored or matched; DisplayName is informational and never used for
// authentication.
type FeishuIdentity struct {
	OpenID      string
	UnionID     string
	TenantKey   string
	Email       string
	DisplayName string
}

// FeishuLoginResult is the outcome of one callback. Exactly one of Session and
// BindToken is set: a session means the browser is signed in, a bind token means
// an existing local account with the same email still has to confirm its
// password.
type FeishuLoginResult struct {
	Session   *Session
	BindToken string
	Email     string
}

// FeishuLogin completes one Feishu callback: it reserves a rate-limit slot for
// the remote address, exchanges the authorization code for a profile, then
// resolves the account in the documented order: an existing binding, an existing
// local account with the same email (which must confirm its password), and only
// then automatic account creation. The callback never logs in an existing local
// account from the Feishu side alone, so a Feishu tenant that controls an email
// address cannot take over a password-protected account.
func (s *Service) FeishuLogin(ctx context.Context, code, remoteIP string) (FeishuLoginResult, error) {
	if !s.FeishuEnabled() {
		return FeishuLoginResult{}, ErrFeishuAuthFailed
	}
	if retryAfter, ok := s.limiter.Take(feishuCallbackRateKey+remoteIP, remoteIP); !ok {
		return FeishuLoginResult{}, RateLimitedError{RetryAfter: retryAfter}
	}
	identity, err := s.fetchFeishuIdentity(ctx, code)
	if err != nil {
		return FeishuLoginResult{}, err
	}
	email, err := normalizeEmail(identity.Email)
	if err != nil {
		return FeishuLoginResult{}, ErrFeishuEmailRequired
	}
	identity.Email = email

	bound, found, err := s.store.UserByFeishuOpenID(ctx, identity.OpenID)
	if err != nil {
		return FeishuLoginResult{}, err
	}
	if found {
		session, err := s.issueFeishuSession(ctx, bound)
		if err != nil {
			return FeishuLoginResult{}, err
		}
		return FeishuLoginResult{Session: &session}, nil
	}
	credential, existing, err := s.store.UserCredential(ctx, email)
	if err != nil {
		return FeishuLoginResult{}, err
	}
	if existing {
		bindToken := feishuBindTokenPrefix + rand.Text()
		s.bindings.put(feishuBindKey(bindToken), feishuPending{
			UserID:    credential.User.ID,
			Email:     email,
			Identity:  identity,
			ExpiresAt: s.clock().Add(FeishuBindLifetime),
		}, s.clock())
		return FeishuLoginResult{BindToken: bindToken, Email: email}, nil
	}
	session, err := s.createFeishuAccount(ctx, identity)
	if err != nil {
		return FeishuLoginResult{}, err
	}
	return FeishuLoginResult{Session: &session}, nil
}

// FeishuBind confirms a pending same-email binding with the account password and
// issues the first real session. The token is claimed before any check runs, so
// concurrent requests carrying the same token cannot both complete it; a failure
// that the user may retry releases the claim, while a completed or unusable
// binding is deleted. A wrong password therefore leaves the pending binding in
// place (until it expires or the window is exhausted). A password-less account
// fails exactly like a wrong password and never gains a binding this way.
func (s *Service) FeishuBind(ctx context.Context, bindToken, password, remoteIP string) (Session, error) {
	key := feishuBindKey(bindToken)
	pending, found := s.bindings.claim(key, s.clock())
	if !found {
		return Session{}, ErrUnauthenticated
	}
	if err := validatePassword(password); err != nil {
		s.bindings.release(key)
		return Session{}, err
	}
	if retryAfter, ok := s.limiter.Take(pending.Email, remoteIP); !ok {
		s.bindings.release(key)
		return Session{}, RateLimitedError{RetryAfter: retryAfter}
	}
	credential, found, err := s.store.UserCredential(ctx, pending.Email)
	if err != nil {
		s.bindings.release(key)
		return Session{}, err
	}
	if !found || credential.User.ID != pending.UserID {
		// The account the callback saw no longer exists, so this pending binding
		// cannot be completed.
		s.bindings.delete(key)
		return Session{}, ErrUnauthenticated
	}
	if err := verifyStoredPassword(credential.PasswordHash, password); err != nil {
		s.bindings.release(key)
		return Session{}, err
	}
	token := newSessionToken()
	var issued IssuedSession
	err = s.tx.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
		if err := tx.UpsertFeishuIdentity(ctx, credential.User.ID, pending.Identity); err != nil {
			return err
		}
		expiresAt, err := tx.CreateSession(ctx, credential.User.ID, sessionTokenHash(token))
		if err != nil {
			return err
		}
		issued = IssuedSession{User: credential.User, ExpiresAt: expiresAt}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrFeishuIdentityConflict) {
			// The open_id belongs to another account now, so this pending binding
			// can never be completed and must not stay usable.
			s.bindings.delete(key)
			return Session{}, ErrUnauthenticated
		}
		// Every other failure is retryable: the account and the binding are
		// unchanged, so the token stays usable with another password attempt.
		s.bindings.release(key)
		return Session{}, err
	}
	s.bindings.delete(key)
	return Session{Token: token, User: issued.User, ExpiresAt: issued.ExpiresAt}, nil
}

// issueFeishuSession creates a session for an already bound account.
func (s *Service) issueFeishuSession(ctx context.Context, user User) (Session, error) {
	token := newSessionToken()
	expiresAt, err := s.store.CreateSession(ctx, user.ID, sessionTokenHash(token))
	if err != nil {
		return Session{}, err
	}
	return Session{Token: token, User: user, ExpiresAt: expiresAt}, nil
}

// createFeishuAccount creates a password-less account, its Feishu binding and its
// first session in one transaction. A closed deployment takes the same
// registration lock and empty-database check as registration: automatic
// provisioning holds the lock too, because it also inserts the account that makes
// the database non-empty, but it skips the check itself since the Feishu tenant
// has already verified the person. Like registration it writes no workspace and
// no membership.
func (s *Service) createFeishuAccount(ctx context.Context, identity FeishuIdentity) (Session, error) {
	token := newSessionToken()
	var issued IssuedSession
	err := s.tx.InTx(ctx, func(ctx context.Context, tx RegistrationStore) error {
		if !s.registrationOpen {
			// The lock is taken whenever registration is closed, including the
			// auto-provisioning path: otherwise a concurrent ordinary registration
			// could pass the empty-database check while this transaction inserts a
			// user, and both would be accepted as the first account.
			if err := tx.LockRegistration(ctx); err != nil {
				return err
			}
			if !s.feishu.AutoProvision {
				taken, err := tx.HasUsers(ctx)
				if err != nil {
					return err
				}
				if taken {
					return ErrRegistrationClosed
				}
			}
		}
		user, err := tx.CreateFeishuUser(ctx, identity.Email)
		if err != nil {
			return err
		}
		if err := tx.UpsertFeishuIdentity(ctx, user.ID, identity); err != nil {
			return err
		}
		expiresAt, err := tx.CreateSession(ctx, user.ID, sessionTokenHash(token))
		if err != nil {
			return err
		}
		issued = IssuedSession{User: user, ExpiresAt: expiresAt}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrFeishuIdentityConflict) {
			return Session{}, ErrFeishuAuthFailed
		}
		return Session{}, err
	}
	return Session{Token: token, User: issued.User, ExpiresAt: issued.ExpiresAt}, nil
}

// feishuEnvelope is one open platform response. The same fields may arrive at
// the top level or under "data" depending on the endpoint version, so readers
// consult both with merged.
type feishuEnvelope struct {
	Code            int    `json:"code"`
	Msg             string `json:"msg"`
	AppAccessToken  string `json:"app_access_token"`
	UserAccessToken string `json:"user_access_token"`
	// AccessToken is the OIDC response spelling of the same token, accepted so
	// the stricter endpoint shape also works.
	AccessToken     string          `json:"access_token"`
	OpenID          string          `json:"open_id"`
	UnionID         string          `json:"union_id"`
	TenantKey       string          `json:"tenant_key"`
	Email           string          `json:"email"`
	EnterpriseEmail string          `json:"enterprise_email"`
	Name            string          `json:"name"`
	Data            *feishuEnvelope `json:"data"`
}

// merged reads one field from the envelope, preferring the top level and falling
// back to the nested "data" object. The provider uses both shapes.
func merged(envelope feishuEnvelope, pick func(feishuEnvelope) string) string {
	if value := pick(envelope); value != "" {
		return value
	}
	if envelope.Data != nil {
		return pick(*envelope.Data)
	}
	return ""
}

// fetchFeishuIdentity exchanges the authorization code for a profile. The
// application token call is best effort: the user-token request carries the
// application credentials itself, so a tenant that rejects the internal token
// endpoint (or a stub that does not implement it) still works.
func (s *Service) fetchFeishuIdentity(ctx context.Context, code string) (FeishuIdentity, error) {
	appToken, err := s.feishuAppToken(ctx)
	if err != nil {
		return FeishuIdentity{}, err
	}
	tokenEnvelope, err := s.feishuUserToken(ctx, code, appToken)
	if err != nil {
		return FeishuIdentity{}, err
	}
	identity := FeishuIdentity{
		OpenID:      merged(tokenEnvelope, openID),
		UnionID:     merged(tokenEnvelope, unionID),
		TenantKey:   merged(tokenEnvelope, tenantKey),
		Email:       firstNonEmpty(merged(tokenEnvelope, feishuEmail), merged(tokenEnvelope, enterpriseEmail)),
		DisplayName: merged(tokenEnvelope, displayName),
	}
	// The token response usually carries only the token; the profile call fills
	// whatever the token response omitted. A response that already holds the
	// open_id and the email needs no second call.
	if identity.OpenID != "" && identity.Email != "" {
		return identity, nil
	}
	userToken := firstNonEmpty(merged(tokenEnvelope, userAccessToken), merged(tokenEnvelope, accessToken))
	if userToken == "" {
		return FeishuIdentity{}, ErrFeishuAuthFailed
	}
	info, err := s.feishuUserInfo(ctx, userToken)
	if err != nil {
		return FeishuIdentity{}, err
	}
	identity.OpenID = firstNonEmpty(identity.OpenID, merged(info, openID))
	identity.UnionID = firstNonEmpty(identity.UnionID, merged(info, unionID))
	identity.TenantKey = firstNonEmpty(identity.TenantKey, merged(info, tenantKey))
	identity.Email = firstNonEmpty(identity.Email, merged(info, enterpriseEmail), merged(info, feishuEmail))
	identity.DisplayName = firstNonEmpty(identity.DisplayName, merged(info, displayName))
	if identity.OpenID == "" {
		return FeishuIdentity{}, ErrFeishuAuthFailed
	}
	return identity, nil
}

// Field readers shared by the token and profile envelopes.
func openID(e feishuEnvelope) string      { return e.OpenID }
func unionID(e feishuEnvelope) string     { return e.UnionID }
func tenantKey(e feishuEnvelope) string   { return e.TenantKey }
func feishuEmail(e feishuEnvelope) string { return e.Email }
func enterpriseEmail(e feishuEnvelope) string {
	return e.EnterpriseEmail
}
func displayName(e feishuEnvelope) string     { return e.Name }
func userAccessToken(e feishuEnvelope) string { return e.UserAccessToken }
func accessToken(e feishuEnvelope) string     { return e.AccessToken }

// feishuAppToken reads the application access token. A non-success response
// yields an empty token instead of an error, because the user-token request can
// still authenticate with the application credentials.
func (s *Service) feishuAppToken(ctx context.Context) (string, error) {
	envelope, status, err := s.feishuPOST(ctx, feishuAppTokenPath, "", map[string]string{
		"app_id":     s.feishu.AppID,
		"app_secret": s.feishu.AppSecret,
	})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK || envelope.Code != 0 {
		return "", nil
	}
	return merged(envelope, func(e feishuEnvelope) string { return e.AppAccessToken }), nil
}

// feishuUserToken exchanges the authorization code for a user access token. The
// OIDC path is preferred; the legacy path is tried when the tenant or stub does
// not offer it.
func (s *Service) feishuUserToken(ctx context.Context, code, appToken string) (feishuEnvelope, error) {
	body := map[string]string{
		"grant_type": "authorization_code",
		"code":       code,
		"app_id":     s.feishu.AppID,
		"app_secret": s.feishu.AppSecret,
	}
	envelope, status, err := s.feishuPOST(ctx, feishuOIDCAccessTokenPath, appToken, body)
	if err == nil && status == http.StatusOK && envelope.Code == 0 {
		if merged(envelope, userAccessToken) != "" {
			return envelope, nil
		}
	}
	legacy, legacyStatus, legacyErr := s.feishuPOST(ctx, feishuAccessTokenPath, appToken, body)
	if legacyErr != nil {
		return feishuEnvelope{}, legacyErr
	}
	if legacyStatus != http.StatusOK || legacy.Code != 0 || merged(legacy, userAccessToken) == "" {
		return feishuEnvelope{}, ErrFeishuAuthFailed
	}
	return legacy, nil
}

// feishuUserInfo reads the profile with the user access token.
func (s *Service) feishuUserInfo(ctx context.Context, userToken string) (feishuEnvelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.feishu.BaseURL+feishuUserInfoPath, nil)
	if err != nil {
		return feishuEnvelope{}, fmt.Errorf("%w: request", ErrFeishuAuthFailed)
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	envelope, status, err := s.feishuDo(req)
	if err != nil {
		return feishuEnvelope{}, err
	}
	if status != http.StatusOK || envelope.Code != 0 {
		return feishuEnvelope{}, ErrFeishuAuthFailed
	}
	return envelope, nil
}

// feishuPOST sends one JSON request and decodes the envelope.
func (s *Service) feishuPOST(ctx context.Context, path, bearer string, body any) (feishuEnvelope, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return feishuEnvelope{}, 0, fmt.Errorf("%w: encode", ErrFeishuAuthFailed)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.feishu.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return feishuEnvelope{}, 0, fmt.Errorf("%w: request", ErrFeishuAuthFailed)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return s.feishuDo(req)
}

// feishuDo performs one request and decodes the response. Transport failures,
// unreadable bodies and malformed JSON all become ErrFeishuAuthFailed with a
// fixed stage only: the provider body may quote the request, so it is never
// wrapped.
func (s *Service) feishuDo(req *http.Request) (feishuEnvelope, int, error) {
	resp, err := s.feishuClient.Do(req)
	if err != nil {
		return feishuEnvelope{}, 0, fmt.Errorf("%w: request", ErrFeishuAuthFailed)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeishuResponseBytes))
	if err != nil {
		return feishuEnvelope{}, resp.StatusCode, fmt.Errorf("%w: read", ErrFeishuAuthFailed)
	}
	var envelope feishuEnvelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return feishuEnvelope{}, resp.StatusCode, fmt.Errorf("%w: decode", ErrFeishuAuthFailed)
		}
	}
	return envelope, resp.StatusCode, nil
}

// firstNonEmpty returns the first non-empty value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// feishuPending is one same-email binding waiting for the account password. It
// keeps the verified profile so the bind request needs no second Feishu call.
// InFlight marks a token one request is already spending, so a second request
// carrying the same token is refused instead of issuing a second session.
type feishuPending struct {
	UserID    ID
	Email     string
	Identity  FeishuIdentity
	ExpiresAt time.Time
	InFlight  bool
}

// feishuBindings is the per-process store of pending bindings. Keys are SHA-256
// digests of the bind token, so the map never holds a usable credential. Like
// the rate limiter it starts no goroutine: expired entries are pruned inside the
// request that touches the map.
type feishuBindings struct {
	mu      sync.Mutex
	entries map[string]feishuPending
}

// newFeishuBindings returns an empty store.
func newFeishuBindings() *feishuBindings {
	return &feishuBindings{entries: make(map[string]feishuPending)}
}

// put stores one pending binding under its digest key.
func (b *feishuBindings) put(key string, pending feishuPending, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	b.entries[key] = pending
}

// claim marks one live pending binding as in flight and returns it. It reports
// false for an unknown, expired or already claimed token, so exactly one of
// several concurrent requests carrying the same token can proceed.
func (b *feishuBindings) claim(key string, now time.Time) (feishuPending, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune(now)
	pending, ok := b.entries[key]
	if !ok || pending.InFlight {
		return feishuPending{}, false
	}
	pending.InFlight = true
	b.entries[key] = pending
	return pending, true
}

// release gives a claimed binding back after a retryable failure, so the same
// token may be used again until it expires.
func (b *feishuBindings) release(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pending, ok := b.entries[key]
	if !ok {
		return
	}
	pending.InFlight = false
	b.entries[key] = pending
}

// delete removes one pending binding after it was completed or became unusable.
func (b *feishuBindings) delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// prune drops every expired entry. It runs under the same mutex that guards the
// map, so the map cannot grow with tokens that are never looked up again.
func (b *feishuBindings) prune(now time.Time) {
	for key, pending := range b.entries {
		if !pending.ExpiresAt.After(now) {
			delete(b.entries, key)
		}
	}
}

// feishuBindKey returns the hex SHA-256 digest that keys a pending binding.
func feishuBindKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// clock reads the clock of the pending-binding expiry.
func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
