package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/usage"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// usageFakeStore returns canned rows and counts statements, so a rejected request
// can be proven to have stopped before SQL.
type usageFakeStore struct {
	calls     int
	aggregate usage.AggregateRows
	rankings  usage.RankingsRows
	err       error
	workspace string
}

func (f *usageFakeStore) Aggregate(ctx context.Context, p usage.AggregateParams) (usage.AggregateRows, error) {
	f.calls++
	f.workspace = p.WorkspaceID.String()
	return f.aggregate, f.err
}

func (f *usageFakeStore) Rankings(ctx context.Context, p usage.RankingsParams) (usage.RankingsRows, error) {
	f.calls++
	f.workspace = p.WorkspaceID.String()
	return f.rankings, f.err
}

type usageFixture struct {
	*credentialFixture
	store *usageFakeStore
}

func newUsageFixture(t *testing.T) *usageFixture {
	t.Helper()
	adapter, authStore := newTestAuth(t)
	user := authStore.seedUser(t, "reader@example.com", "synthetic-password")
	roles := newRoleFakeStore()
	store := &usageFakeStore{}
	handler := NewHandler(Options{
		CheckReady: func(context.Context) error { return nil },
		Auth:       adapter,
		Usage:      NewUsage(usage.New(store), workspaces.New(roles)),
	})
	login := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(`{"email":"reader@example.com","password":"synthetic-password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", testOrigin)
	login.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, login)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d", recorder.Code)
	}
	f := &credentialFixture{
		handler: handler, roles: roles, authStore: authStore, ownerCookie: sessionCookie(t, recorder),
		ownerID: workspaces.ID{Bytes: user.ID, Valid: true}, workspaceID: idWithByte(0x41), otherSpace: idWithByte(0x42),
	}
	return &usageFixture{credentialFixture: f, store: store}
}

func (f *usageFixture) path(kind, query string) string {
	return "/api/v1/workspaces/" + f.workspaceID.String() + "/usage/" + kind + "?" + query
}

const (
	validAggregateQuery = "start=2026-01-01T00:00:00Z&end=2026-01-01T03:00:00Z&timezone=UTC&granularity=hour"
	validRankingsQuery  = "start=2026-01-01T00:00:00Z&end=2026-01-01T03:00:00Z&group_by=model"
)

func (f *usageFixture) get(t *testing.T, path string, cookie bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie {
		req.AddCookie(f.ownerCookie)
	}
	return f.doRequest(t, req)
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Code, body.Error.Details
}

func assertNoStore(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Get("Cache-Control"); got != noStoreValue {
		t.Fatalf("Cache-Control = %q (status %d)", got, w.Code)
	}
}

func TestUsageRequiresSession(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	for _, kind := range []string{"aggregate", "rankings"} {
		q := validAggregateQuery
		if kind == "rankings" {
			q = validRankingsQuery
		}
		w := f.get(t, f.path(kind, q), false)
		if code, _ := errorCode(t, w); w.Code != http.StatusUnauthorized || code != "UNAUTHENTICATED" {
			t.Fatalf("%s: status %d code %s", kind, w.Code, code)
		}
		assertNoStore(t, w)
	}
	if f.store.calls != 0 || f.roles.callCount() != 0 {
		t.Fatal("an unauthenticated request must not look up membership or query")
	}
}

func TestUsageMembershipRules(t *testing.T) {
	cases := []struct {
		name   string
		role   workspaces.Role
		member bool
		status int
		code   string
		query  bool
	}{
		{"owner reads", workspaces.RoleOwner, true, http.StatusOK, "", true},
		{"member reads", workspaces.RoleMember, true, http.StatusOK, "", true},
		{"non member is denied", "", false, http.StatusForbidden, "PERMISSION_DENIED", false},
		{"unknown role is an internal error, not allowed", "viewer", true, http.StatusInternalServerError, "INTERNAL", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageFixture(t)
			if tc.member {
				f.roles.setRole(f.ownerID, f.workspaceID, tc.role)
			}
			f.store.calls = 0
			w := f.get(t, f.path("aggregate", validAggregateQuery), true)
			if w.Code != tc.status {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if tc.code != "" {
				if code, _ := errorCode(t, w); code != tc.code {
					t.Fatalf("code = %s", code)
				}
			}
			if (f.store.calls == 1) != tc.query {
				t.Fatalf("store calls = %d", f.store.calls)
			}
			assertNoStore(t, w)
			if f.store.calls == 1 && f.store.workspace != f.workspaceID.String() {
				t.Fatal("store got another workspace")
			}
		})
	}
}

func TestUsageMembershipLookupFailureIsInternal(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setError(errors.New("database down"))
	w := f.get(t, f.path("rankings", validRankingsQuery), true)
	if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || f.store.calls != 0 {
		t.Fatalf("status %d code %s calls %d", w.Code, code, f.store.calls)
	}
	if strings.Contains(w.Body.String(), "database down") {
		t.Fatal("error leaked")
	}
}

func TestUsageMalformedWorkspaceID(t *testing.T) {
	f := newUsageFixture(t)
	w := f.get(t, "/api/v1/workspaces/nope/usage/aggregate?"+validAggregateQuery, true)
	if code, _ := errorCode(t, w); w.Code != http.StatusBadRequest || code != "INVALID_ARGUMENT" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
}

func TestUsageInvalidArgumentsDoNotQuery(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleMember)
	bad := []string{
		validAggregateQuery + "&extra=1",
		validAggregateQuery + "&start=2026-01-01T00:00:00Z",
		"start=2026-01-01T00:00:01Z&end=2026-01-01T03:00:00Z&timezone=UTC&granularity=hour",
		validAggregateQuery + "&provider=",
		validAggregateQuery + "&project=nope",
		validAggregateQuery + "&provider=%ff",
		validAggregateQuery + "&provider=%zz",
		validAggregateQuery + ";x=1",
		"start=2026-01-01T00:00:00Z&end=2026-01-01T03:00:00Z&timezone=Local&granularity=hour",
	}
	for _, q := range bad {
		w := f.get(t, f.path("aggregate", q), true)
		code, _ := errorCode(t, w)
		if w.Code != http.StatusBadRequest || code != "INVALID_ARGUMENT" {
			t.Fatalf("%q: status %d code %s", q, w.Code, code)
		}
		assertNoStore(t, w)
		if strings.Contains(w.Body.String(), "extra") {
			t.Fatal("request value echoed")
		}
	}
	if w := f.get(t, f.path("rankings", validRankingsQuery+"&limit=51"), true); w.Code != http.StatusBadRequest {
		t.Fatalf("limit 51 status = %d", w.Code)
	}
	if f.store.calls != 0 {
		t.Fatal("invalid requests reached the store")
	}
}

func TestUsageOutsideRetentionDetails(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	w := f.get(t, f.path("aggregate", "start=2000-01-01T00:00:00Z&end=2000-01-01T03:00:00Z&timezone=UTC&granularity=hour"), true)
	code, details := errorCode(t, w)
	if w.Code != http.StatusBadRequest || code != "OUTSIDE_RETENTION" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	cutoff, _ := details["retention_cutoff"].(string)
	parsed, err := time.Parse(time.RFC3339, cutoff)
	if err != nil || parsed.Second() != 0 || !strings.HasSuffix(cutoff, "Z") {
		t.Fatalf("retention_cutoff = %q", cutoff)
	}
	if f.store.calls != 0 {
		t.Fatal("retention violation reached the store")
	}
	assertNoStore(t, w)
}

func TestUsageTooManyPointsDetails(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	start := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Minute)
	q := "start=" + start.Format(time.RFC3339) + "&timezone=UTC&granularity=minute"
	w := f.get(t, f.path("aggregate", q), true)
	code, details := errorCode(t, w)
	if w.Code != http.StatusBadRequest || code != "TOO_MANY_POINTS" {
		t.Fatalf("status %d code %s: %s", w.Code, code, w.Body.String())
	}
	if details["max_points"] != float64(1000) || details["suggested_granularity"] != "hour" {
		t.Fatalf("details = %v", details)
	}
	// Day granularity over 13 months fits; a finer request over a long span may
	// have no suggestion, which must be an explicit JSON null.
	if !strings.Contains(w.Body.String(), `"max_points":1000`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	if f.store.calls != 0 {
		t.Fatal("too many points reached the store")
	}
}

func TestUsageStringsAndShapesOnSuccess(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleMember)
	huge := "123456789012345678901234567890"
	row := usage.RawStats{RequestCount: "2"}
	for i := range row.Metrics {
		row.Metrics[i] = usage.RawMetric{Sum: huge, Known: "1", Estimated: "0", Missing: "1"}
	}
	row.Metrics[4] = usage.RawMetric{Sum: "0", Known: "0", Estimated: "0", Missing: "2"}
	f.store.aggregate = usage.AggregateRows{Overall: &row, Points: []usage.RawPoint{{Index: 0, Stats: row}}}
	w := f.get(t, f.path("aggregate", validAggregateQuery), true)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	assertNoStore(t, w)
	body := w.Body.String()
	for _, want := range []string{`"request_count":"2"`, `"value":"` + huge + `"`, `"cache_write_tokens":{"value":null,"known_count":"0","estimated_count":"0","missing_count":"2"}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %s: %s", want, body)
		}
	}
	// Empty rankings keep arrays and nulls explicit.
	f.store.rankings = usage.RankingsRows{Overall: usage.RawStats{RequestCount: "0", Metrics: zeroMetrics()}}
	w = f.get(t, f.path("rankings", validRankingsQuery), true)
	body = w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"groups":[]`) || !strings.Contains(body, `"other":null`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Fatalf("empty rankings: %d %s", w.Code, body)
	}
}

func zeroMetrics() [6]usage.RawMetric {
	var m [6]usage.RawMetric
	for i := range m {
		m[i] = usage.RawMetric{Sum: "0", Known: "0", Estimated: "0", Missing: "0"}
	}
	return m
}

func TestUsageStoreFailureIsInternalWithoutLeak(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	f.store.err = errors.New("SELECT secret FROM usage_minute_stats")
	for _, kind := range []string{"aggregate", "rankings"} {
		q := validAggregateQuery
		if kind == "rankings" {
			q = validRankingsQuery
		}
		w := f.get(t, f.path(kind, q), true)
		if code, _ := errorCode(t, w); w.Code != http.StatusInternalServerError || code != "INTERNAL" || strings.Contains(w.Body.String(), "SELECT") {
			t.Fatalf("%s: %d %s", kind, w.Code, w.Body.String())
		}
	}
}

func TestUsageSessionUnavailableIs503(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	f.authStore.failures.authenticate = errors.Join(auth.ErrUnavailable, errors.New("synthetic"))
	w := f.get(t, f.path("aggregate", validAggregateQuery), true)
	if code, _ := errorCode(t, w); w.Code != http.StatusServiceUnavailable || code != "UNAVAILABLE" {
		t.Fatalf("status %d code %s", w.Code, code)
	}
	assertNoStore(t, w)
}

func TestUsageRoutesAbsentWithoutDependencies(t *testing.T) {
	adapter, _ := newTestAuth(t)
	roles := newRoleFakeStore()
	path := "/api/v1/workspaces/" + idWithByte(1).String() + "/usage/aggregate?" + validAggregateQuery
	handlers := map[string]http.Handler{
		"no auth":       NewHandler(Options{Usage: NewUsage(usage.New(&usageFakeStore{}), workspaces.New(roles))}),
		"no service":    NewHandler(Options{Auth: adapter, Usage: NewUsage(nil, workspaces.New(roles))}),
		"no membership": NewHandler(Options{Auth: adapter, Usage: NewUsage(usage.New(&usageFakeStore{}), nil)}),
		"no usage":      NewHandler(Options{Auth: adapter}),
	}
	for name, h := range handlers {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (route absent)", name, w.Code)
		}
	}
}

func TestUsageRejectsNonGet(t *testing.T) {
	f := newUsageFixture(t)
	f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
	req := httptest.NewRequest(http.MethodPost, f.path("aggregate", validAggregateQuery), nil)
	req.AddCookie(f.ownerCookie)
	if w := f.doRequest(t, req); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestWriteUsageErrorTooManyPointsMessage(t *testing.T) {
	hour := usage.Hour
	for name, tc := range map[string]struct {
		suggested *usage.Granularity
		message   string
	}{
		"no suggestion":   {nil, "time range has too many points; shorten the time range"},
		"with suggestion": {&hour, "time range has too many points"},
	} {
		w := httptest.NewRecorder()
		writeUsageError(httptest.NewRequest(http.MethodGet, "/", nil), w, &usage.TooManyPointsError{MaxPoints: 1000, Suggested: tc.suggested})
		var body struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusBadRequest || body.Error.Code != "TOO_MANY_POINTS" || body.Error.Message != tc.message {
			t.Fatalf("%s: status %d body %s", name, w.Code, w.Body.String())
		}
		want := any(nil)
		if tc.suggested != nil {
			want = "hour"
		}
		if body.Error.Details["max_points"] != float64(1000) || body.Error.Details["suggested_granularity"] != want {
			t.Fatalf("%s: details = %v", name, body.Error.Details)
		}
	}
}
