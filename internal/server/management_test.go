package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/projects"
	"github.com/dundunHa/TokenPivot/internal/sources"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// projectFakeStore is an in-memory project store that scopes every call by
// workspace and counts the calls that reach it.
type projectFakeStore struct {
	mu    sync.Mutex
	rows  []projects.Project
	calls int
	err   error
}

func (f *projectFakeStore) List(ctx context.Context, workspaceID projects.ID) ([]projects.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	list := []projects.Project{}
	for _, row := range f.rows {
		if row.WorkspaceID.Bytes == workspaceID.Bytes {
			list = append(list, row)
		}
	}
	return list, nil
}

func (f *projectFakeStore) Create(ctx context.Context, workspaceID projects.ID, key, name string) (projects.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return projects.Project{}, f.err
	}
	for _, row := range f.rows {
		if row.WorkspaceID.Bytes == workspaceID.Bytes && row.Key == key {
			return projects.Project{}, projects.ErrConflict
		}
	}
	project := projects.Project{ID: idWithByte(byte(0x80 + len(f.rows))), WorkspaceID: workspaceID, Key: key, Name: name, CreatedAt: time.Unix(1700000000, 0)}
	f.rows = append(f.rows, project)
	return project, nil
}

func (f *projectFakeStore) Update(ctx context.Context, workspaceID, projectID projects.ID, name string) (projects.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return projects.Project{}, f.err
	}
	for i, row := range f.rows {
		if row.ID.Bytes == projectID.Bytes && row.WorkspaceID.Bytes == workspaceID.Bytes {
			f.rows[i].Name = name
			return f.rows[i], nil
		}
	}
	return projects.Project{}, projects.ErrNotFound
}

func (f *projectFakeStore) Archive(ctx context.Context, workspaceID, projectID projects.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	for i, row := range f.rows {
		if row.ID.Bytes == projectID.Bytes && row.WorkspaceID.Bytes == workspaceID.Bytes {
			if f.rows[i].ArchivedAt == nil {
				stamp := time.Unix(1700000100, 0)
				f.rows[i].ArchivedAt = &stamp
			}
			return nil
		}
	}
	return projects.ErrNotFound
}

func (f *projectFakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sourceFakeStore is the source counterpart of projectFakeStore.
type sourceFakeStore struct {
	mu       sync.Mutex
	rows     []sources.Source
	calls    int
	err      error
	archived func(sourceID sources.ID)
}

func (f *sourceFakeStore) List(ctx context.Context, workspaceID sources.ID) ([]sources.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	list := []sources.Source{}
	for _, row := range f.rows {
		if row.WorkspaceID.Bytes == workspaceID.Bytes {
			list = append(list, row)
		}
	}
	return list, nil
}

func (f *sourceFakeStore) Create(ctx context.Context, workspaceID sources.ID, platform, instanceKey string) (sources.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return sources.Source{}, f.err
	}
	for _, row := range f.rows {
		if row.WorkspaceID.Bytes == workspaceID.Bytes && row.Platform == platform && row.InstanceKey == instanceKey {
			return sources.Source{}, sources.ErrConflict
		}
	}
	source := sources.Source{ID: idWithByte(byte(0xa0 + len(f.rows))), WorkspaceID: workspaceID, Platform: platform, InstanceKey: instanceKey, CreatedAt: time.Unix(1700000000, 0)}
	f.rows = append(f.rows, source)
	return source, nil
}

func (f *sourceFakeStore) Update(ctx context.Context, workspaceID, sourceID sources.ID, platform, instanceKey *string) (sources.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return sources.Source{}, f.err
	}
	for i, row := range f.rows {
		if row.ID.Bytes == sourceID.Bytes && row.WorkspaceID.Bytes == workspaceID.Bytes {
			if platform != nil {
				f.rows[i].Platform = *platform
			}
			if instanceKey != nil {
				f.rows[i].InstanceKey = *instanceKey
			}
			return f.rows[i], nil
		}
	}
	return sources.Source{}, sources.ErrNotFound
}

func (f *sourceFakeStore) Archive(ctx context.Context, workspaceID, sourceID sources.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	for i, row := range f.rows {
		if row.ID.Bytes == sourceID.Bytes && row.WorkspaceID.Bytes == workspaceID.Bytes {
			if f.rows[i].ArchivedAt == nil {
				stamp := time.Unix(1700000100, 0)
				f.rows[i].ArchivedAt = &stamp
			}
			if f.archived != nil {
				f.archived(sourceID)
			}
			return nil
		}
	}
	return sources.ErrNotFound
}

func (f *sourceFakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// managementFixture wires projects, sources and credentials behind the real
// Auth adapter, so Origin, session and role gates run end to end.
type managementFixture struct {
	*credentialFixture
	projects *projectFakeStore
	sources  *sourceFakeStore
}

func newManagementFixture(t *testing.T) *managementFixture {
	t.Helper()
	adapter, authStore := newTestAuth(t)
	user := authStore.seedUser(t, "owner@example.com", "synthetic-password")
	credStore := newCredentialFakeStore()
	roles := newRoleFakeStore()
	projectStore := &projectFakeStore{}
	sourceStore := &sourceFakeStore{archived: func(id sources.ID) {
		credStore.mu.Lock()
		defer credStore.mu.Unlock()
		credStore.archived[id.Bytes] = true
	}}
	memberships := workspaces.New(roles)
	service := credentials.New(credStore)
	handler := NewHandler(Options{
		Auth:        adapter,
		Credentials: NewCredentials(service, memberships),
		Projects:    NewProjects(projects.New(projectStore), memberships),
		Sources:     NewSources(sources.New(sourceStore), memberships),
	})
	login := httptest.NewRequest(http.MethodPost, loginPath, strings.NewReader(`{"email":"owner@example.com","password":"synthetic-password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", testOrigin)
	login.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, login)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d", recorder.Code)
	}
	base := &credentialFixture{
		handler: handler, store: credStore, roles: roles, service: service, authStore: authStore,
		ownerCookie: sessionCookie(t, recorder), ownerID: workspaces.ID{Bytes: user.ID, Valid: true},
		workspaceID: idWithByte(0x21), otherSpace: idWithByte(0x22), sourceID: idWithByte(0x31), otherSource: idWithByte(0x32),
	}
	roles.setRole(base.ownerID, base.workspaceID, workspaces.RoleOwner)
	credStore.addSource(base.workspaceID, base.sourceID)
	credStore.addSource(base.otherSpace, base.otherSource)
	return &managementFixture{credentialFixture: base, projects: projectStore, sources: sourceStore}
}

func (f *managementFixture) wsPath(workspace workspaces.ID, rest string) string {
	return "/api/v1/workspaces/" + workspace.String() + rest
}

// jsonRequest builds an owner request with a JSON content type.
func (f *managementFixture) jsonRequest(method, path, body string) *http.Request {
	req := f.newRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (f *managementFixture) seedProject(workspace workspaces.ID, key string) projects.Project {
	project, err := f.projects.Create(context.Background(), workspace, key, "Name "+key)
	if err != nil {
		panic(err)
	}
	return project
}

func (f *managementFixture) seedSource(workspace workspaces.ID, platform, instance string) sources.Source {
	source, err := f.sources.Create(context.Background(), workspace, platform, instance)
	if err != nil {
		panic(err)
	}
	return source
}

type routeCase struct {
	name   string
	method string
	path   string
	body   string
	write  bool
	okCode int
}

// routes lists every project and source route for the fixture workspace.
func (f *managementFixture) routes(project projects.Project, source sources.Source) []routeCase {
	return []routeCase{
		{"projects list", http.MethodGet, f.wsPath(f.workspaceID, "/projects"), "", false, 200},
		{"projects create", http.MethodPost, f.wsPath(f.workspaceID, "/projects"), `{"project_key":"new","name":"New"}`, true, 201},
		{"projects update", http.MethodPatch, f.wsPath(f.workspaceID, "/projects/"+project.ID.String()), `{"name":"Renamed"}`, true, 200},
		{"projects archive", http.MethodDelete, f.wsPath(f.workspaceID, "/projects/"+project.ID.String()), "", true, 204},
		{"sources list", http.MethodGet, f.wsPath(f.workspaceID, "/sources"), "", false, 200},
		{"sources create", http.MethodPost, f.wsPath(f.workspaceID, "/sources"), `{"platform":"dsh","instance_key":"new"}`, true, 201},
		{"sources update", http.MethodPatch, f.wsPath(f.workspaceID, "/sources/"+source.ID.String()), `{"instance_key":"renamed"}`, true, 200},
		{"sources archive", http.MethodDelete, f.wsPath(f.workspaceID, "/sources/"+source.ID.String()), "", true, 204},
	}
}

func (f *managementFixture) storeCalls() int {
	return f.projects.callCount() + f.sources.callCount()
}

func TestManagementRoleMatrix(t *testing.T) {
	f := newManagementFixture(t)
	project := f.seedProject(f.workspaceID, "p1")
	source := f.seedSource(f.workspaceID, "dsh", "i1")
	base := f.storeCalls()
	for _, route := range f.routes(project, source) {
		t.Run(route.name, func(t *testing.T) {
			// Anonymous: 401 (Origin passes), never reaches a store.
			anonymous := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			anonymous.Header.Set("Origin", testOrigin)
			anonymous.Header.Set("Content-Type", "application/json")
			if w := f.doRequest(t, anonymous); w.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous = %d", w.Code)
			}
			// Non-member: 403 for reads and writes.
			f.roles.clearRoles()
			if w := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "PERMISSION_DENIED") {
				t.Fatalf("non-member = %d %s", w.Code, w.Body.String())
			}
			// Member: reads pass, writes are 403.
			f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleMember)
			member := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body))
			if route.write && member.Code != http.StatusForbidden {
				t.Fatalf("member write = %d", member.Code)
			}
			if !route.write && member.Code != http.StatusOK {
				t.Fatalf("member read = %d", member.Code)
			}
			if got := member.Header().Get("Cache-Control"); got != noStoreValue {
				t.Fatalf("Cache-Control = %q", got)
			}
			f.roles.setRole(f.ownerID, f.workspaceID, workspaces.RoleOwner)
		})
	}
	// Only the two member reads may have reached a store.
	if got := f.storeCalls() - base; got != 2 {
		t.Fatalf("denied requests reached stores %d times, want 2 member reads", got)
	}
	// Owner succeeds on every route.
	for _, route := range f.routes(project, source) {
		if w := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body)); w.Code != route.okCode {
			t.Fatalf("owner %s = %d %s", route.name, w.Code, w.Body.String())
		}
	}
}

func TestManagementOriginMatrix(t *testing.T) {
	f := newManagementFixture(t)
	project := f.seedProject(f.workspaceID, "p1")
	source := f.seedSource(f.workspaceID, "dsh", "i1")
	credential := f.store.seedCredential(f.workspaceID, f.sourceID, nil)
	writes := f.routes(project, source)[:0]
	for _, route := range f.routes(project, source) {
		if route.write {
			writes = append(writes, route)
		}
	}
	writes = append(writes,
		routeCase{"credential issue", http.MethodPost, f.collectionPath(), "", true, 201},
		routeCase{"credential rotate", http.MethodPost, f.rotatePath(credential.ID.String()), "", true, 201},
		routeCase{"credential revoke", http.MethodPost, f.revokePath(credential.ID.String()), "", true, 204},
	)
	origins := map[string][]string{
		"missing":  nil,
		"wrong":    {"https://evil.example.com"},
		"null":     {"null"},
		"multiple": {testOrigin, testOrigin},
	}
	for _, route := range writes {
		for name, values := range origins {
			t.Run(route.name+"/"+name, func(t *testing.T) {
				before := f.storeCalls() + f.store.callCount("Insert") + f.store.callCount("Revoke")
				for _, anonymous := range []bool{false, true} {
					req := f.jsonRequest(route.method, route.path, route.body)
					req.Header.Del("Origin")
					for _, v := range values {
						req.Header.Add("Origin", v)
					}
					if anonymous {
						req = httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
						for _, v := range values {
							req.Header.Add("Origin", v)
						}
					}
					if w := f.doRequest(t, req); w.Code != http.StatusForbidden {
						t.Fatalf("anonymous=%v status = %d", anonymous, w.Code)
					}
				}
				if after := f.storeCalls() + f.store.callCount("Insert") + f.store.callCount("Revoke"); after != before {
					t.Fatal("rejected Origin reached storage")
				}
			})
		}
	}
	// Reads work without Origin.
	for _, path := range []string{f.wsPath(f.workspaceID, "/projects"), f.wsPath(f.workspaceID, "/sources")} {
		req := f.newRequest(http.MethodGet, path, "")
		req.Header.Del("Origin")
		if w := f.doRequest(t, req); w.Code != http.StatusOK {
			t.Fatalf("GET without Origin = %d", w.Code)
		}
	}
}

func TestManagementDependencyFailures(t *testing.T) {
	f := newManagementFixture(t)
	project := f.seedProject(f.workspaceID, "p1")
	source := f.seedSource(f.workspaceID, "dsh", "i1")
	routes := f.routes(project, source)

	t.Run("role lookup failure is 500 never 403", func(t *testing.T) {
		f.roles.setError(errors.New("synthetic database failure"))
		defer f.roles.setError(nil)
		for _, route := range routes {
			w := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body))
			if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "INTERNAL") {
				t.Fatalf("%s = %d %s", route.name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "synthetic") {
				t.Fatal("database message leaked")
			}
		}
	})
	t.Run("resource failure is 500", func(t *testing.T) {
		f.projects.err, f.sources.err = errors.New("synthetic database failure"), errors.New("synthetic database failure")
		defer func() { f.projects.err, f.sources.err = nil, nil }()
		for _, route := range routes {
			if w := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body)); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "INTERNAL") {
				t.Fatalf("%s = %d %s", route.name, w.Code, w.Body.String())
			}
		}
	})
	t.Run("session failure is 503 and stops before roles", func(t *testing.T) {
		f.authStore.failures.authenticate = errors.Join(auth.ErrUnavailable, errors.New("synthetic database failure"))
		defer func() { f.authStore.failures.authenticate = nil }()
		calls := f.roles.callCount()
		for _, route := range routes {
			if w := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body)); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "UNAVAILABLE") {
				t.Fatalf("%s = %d %s", route.name, w.Code, w.Body.String())
			}
		}
		if f.roles.callCount() != calls {
			t.Fatal("session failure reached the role lookup")
		}
	})
}

func TestManagementCrossWorkspaceIsNotFound(t *testing.T) {
	f := newManagementFixture(t)
	foreignProject := f.seedProject(f.otherSpace, "foreign")
	foreignSource := f.seedSource(f.otherSpace, "dsh", "foreign")
	for _, route := range []routeCase{
		{"project update", http.MethodPatch, f.wsPath(f.workspaceID, "/projects/"+foreignProject.ID.String()), `{"name":"x"}`, true, 0},
		{"project archive", http.MethodDelete, f.wsPath(f.workspaceID, "/projects/"+foreignProject.ID.String()), "", true, 0},
		{"source update", http.MethodPatch, f.wsPath(f.workspaceID, "/sources/"+foreignSource.ID.String()), `{"platform":"x"}`, true, 0},
		{"source archive", http.MethodDelete, f.wsPath(f.workspaceID, "/sources/"+foreignSource.ID.String()), "", true, 0},
		{"credential issue", http.MethodPost, f.wsPath(f.workspaceID, "/sources/"+f.otherSource.String()+"/credentials"), "", true, 0},
	} {
		w := f.doRequest(t, f.jsonRequest(route.method, route.path, route.body))
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "NOT_FOUND") {
			t.Fatalf("%s = %d %s", route.name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), f.otherSpace.String()) {
			t.Fatalf("%s leaked the foreign workspace", route.name)
		}
	}
	// A foreign credential cannot be rotated through this workspace either.
	foreign := f.store.seedCredential(f.otherSpace, f.otherSource, nil)
	if w := f.do(t, http.MethodPost, f.rotatePath(foreign.ID.String()), ""); w.Code != http.StatusNotFound {
		t.Fatalf("foreign rotate = %d", w.Code)
	}
	// Target workspace the user does not belong to: 403 before any lookup.
	if w := f.do(t, http.MethodGet, f.wsPath(f.otherSpace, "/projects"), ""); w.Code != http.StatusForbidden {
		t.Fatalf("unjoined workspace = %d", w.Code)
	}
	if got := len(f.projects.rows) + len(f.sources.rows); got != 2 {
		t.Fatalf("foreign rows changed: %d", got)
	}
	for _, row := range f.projects.rows {
		if row.ArchivedAt != nil || row.Name == "x" {
			t.Fatal("foreign project changed")
		}
	}
}

func TestManagementRequestValidation(t *testing.T) {
	f := newManagementFixture(t)
	project := f.seedProject(f.workspaceID, "p1")
	source := f.seedSource(f.workspaceID, "dsh", "i1")
	projectsPath := f.wsPath(f.workspaceID, "/projects")
	sourcePath := f.wsPath(f.workspaceID, "/sources/"+source.ID.String())
	long := strings.Repeat("a", 129)
	cases := []struct {
		name         string
		method, path string
		contentType  string
		body         string
		want         int
	}{
		{"empty body", http.MethodPost, projectsPath, "application/json", "", 400},
		{"not json type", http.MethodPost, projectsPath, "text/plain", `{"project_key":"a","name":"b"}`, 415},
		{"missing type", http.MethodPost, projectsPath, "", `{"project_key":"a","name":"b"}`, 415},
		{"too large", http.MethodPost, projectsPath, "application/json", `{"project_key":"a","name":"` + strings.Repeat("x", 16<<10) + `"}`, 413},
		{"unknown field", http.MethodPost, projectsPath, "application/json", `{"project_key":"a","name":"b","workspace_id":"x"}`, 400},
		{"case-different key", http.MethodPost, projectsPath, "application/json", `{"Project_Key":"a","name":"b"}`, 400},
		{"duplicate key", http.MethodPost, projectsPath, "application/json", `{"project_key":"a","project_key":"c","name":"b"}`, 400},
		{"trailing json", http.MethodPost, projectsPath, "application/json", `{"project_key":"a","name":"b"}{}`, 400},
		{"null field", http.MethodPost, projectsPath, "application/json", `{"project_key":null,"name":"b"}`, 400},
		{"wrong type", http.MethodPost, projectsPath, "application/json", `{"project_key":1,"name":"b"}`, 400},
		{"missing field", http.MethodPost, projectsPath, "application/json", `{"project_key":"a"}`, 400},
		{"invalid utf8", http.MethodPost, projectsPath, "application/json", "{\"project_key\":\"a\xff\",\"name\":\"b\"}", 400},
		{"control char", http.MethodPost, projectsPath, "application/json", `{"project_key":"a\u0001","name":"b"}`, 400},
		{"leading space", http.MethodPost, projectsPath, "application/json", `{"project_key":" a","name":"b"}`, 400},
		{"trailing nbsp", http.MethodPost, projectsPath, "application/json", `{"project_key":"a\u00a0","name":"b"}`, 400},
		{"too long", http.MethodPost, projectsPath, "application/json", `{"project_key":"` + long + `","name":"b"}`, 400},
		{"empty value", http.MethodPost, projectsPath, "application/json", `{"project_key":"","name":"b"}`, 400},
		{"project patch key", http.MethodPatch, projectsPath + "/" + project.ID.String(), "application/json", `{"project_key":"z"}`, 400},
		{"source patch empty", http.MethodPatch, sourcePath, "application/json", `{}`, 400},
		{"source patch null", http.MethodPatch, sourcePath, "application/json", `{"platform":null}`, 400},
		{"bad project uuid", http.MethodPatch, projectsPath + "/nope", "application/json", `{"name":"b"}`, 400},
		{"bad workspace uuid", http.MethodGet, "/api/v1/workspaces/nope/projects", "", "", 400},
		{"delete with body", http.MethodDelete, sourcePath, "application/json", `{"force":true}`, 400},
	}
	base := f.storeCalls()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := f.newRequest(tc.method, tc.path, tc.body)
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			w := f.doRequest(t, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "workspace_id") {
				t.Fatal("response echoed a request field")
			}
		})
	}
	if f.storeCalls() != base {
		t.Fatal("a rejected request reached a store")
	}
}

type projectBody struct {
	ProjectID   string  `json:"project_id"`
	WorkspaceID string  `json:"workspace_id"`
	ProjectKey  string  `json:"project_key"`
	Name        string  `json:"name"`
	ArchivedAt  *string `json:"archived_at"`
}

type sourceBody struct {
	SourceID    string  `json:"source_id"`
	WorkspaceID string  `json:"workspace_id"`
	Platform    string  `json:"platform"`
	InstanceKey string  `json:"instance_key"`
	ArchivedAt  *string `json:"archived_at"`
}

func TestProjectLifecycleAndRepeats(t *testing.T) {
	f := newManagementFixture(t)
	listPath := f.wsPath(f.workspaceID, "/projects")
	empty := f.do(t, http.MethodGet, listPath, "")
	if empty.Code != 200 || strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatalf("empty list = %d %s", empty.Code, empty.Body.String())
	}
	create := f.doRequest(t, f.jsonRequest(http.MethodPost, listPath, `{"project_key":"k","name":"N"}`))
	if create.Code != 201 {
		t.Fatalf("create = %d", create.Code)
	}
	var created projectBody
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.WorkspaceID != f.workspaceID.String() || created.ArchivedAt != nil {
		t.Fatalf("created body: %s", create.Body.String())
	}
	if w := f.doRequest(t, f.jsonRequest(http.MethodPost, listPath, `{"project_key":"k","name":"N"}`)); w.Code != 409 || !strings.Contains(w.Body.String(), "CONFLICT") {
		t.Fatalf("replayed POST = %d %s", w.Code, w.Body.String())
	}
	item := listPath + "/" + created.ProjectID
	for i := 0; i < 2; i++ {
		if w := f.doRequest(t, f.jsonRequest(http.MethodPatch, item, `{"name":"Same"}`)); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Same"`) {
			t.Fatalf("patch #%d = %d", i, w.Code)
		}
	}
	for i := 0; i < 2; i++ {
		if w := f.doRequest(t, f.jsonRequest(http.MethodDelete, item, "")); w.Code != 204 || w.Body.Len() != 0 {
			t.Fatalf("delete #%d = %d", i, w.Code)
		}
	}
	list := f.do(t, http.MethodGet, listPath, "")
	var items []projectBody
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0].ArchivedAt == nil {
		t.Fatalf("archived project must stay listed: %s", list.Body.String())
	}
	if w := f.doRequest(t, f.jsonRequest(http.MethodPost, listPath, `{"project_key":"k","name":"Again"}`)); w.Code != 409 {
		t.Fatalf("recreate after archive = %d", w.Code)
	}
}

func TestSourceLifecycleAndArchiveBlocksCredentials(t *testing.T) {
	f := newManagementFixture(t)
	listPath := f.wsPath(f.workspaceID, "/sources")
	create := f.doRequest(t, f.jsonRequest(http.MethodPost, listPath, `{"platform":"dsh","instance_key":"a"}`))
	if create.Code != 201 {
		t.Fatalf("create = %d %s", create.Code, create.Body.String())
	}
	var created sourceBody
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ArchivedAt != nil {
		t.Fatal("bad create body")
	}
	if !strings.Contains(create.Body.String(), `"last_received_at":null`) {
		t.Fatalf("missing nullable fields: %s", create.Body.String())
	}
	if w := f.doRequest(t, f.jsonRequest(http.MethodPost, listPath, `{"platform":"dsh","instance_key":"a"}`)); w.Code != 409 {
		t.Fatalf("replayed POST = %d", w.Code)
	}
	item := listPath + "/" + created.SourceID
	w := f.doRequest(t, f.jsonRequest(http.MethodPatch, item, `{"platform":"pi"}`))
	var patched sourceBody
	if err := json.Unmarshal(w.Body.Bytes(), &patched); err != nil || w.Code != 200 || patched.Platform != "pi" || patched.InstanceKey != "a" || patched.SourceID != created.SourceID {
		t.Fatalf("partial patch = %d %s", w.Code, w.Body.String())
	}
	// Archive twice, then credential issue on the archived source is 409.
	var sid credentials.ID
	if err := sid.Scan(created.SourceID); err != nil {
		t.Fatal(err)
	}
	f.store.addSource(f.workspaceID, sid)
	if w := f.do(t, http.MethodPost, f.wsPath(f.workspaceID, "/sources/"+created.SourceID+"/credentials"), ""); w.Code != 201 {
		t.Fatalf("issue before archive = %d", w.Code)
	}
	for i := 0; i < 2; i++ {
		if w := f.doRequest(t, f.jsonRequest(http.MethodDelete, item, "")); w.Code != 204 {
			t.Fatalf("delete #%d = %d", i, w.Code)
		}
	}
	issue := f.do(t, http.MethodPost, f.wsPath(f.workspaceID, "/sources/"+created.SourceID+"/credentials"), "")
	if issue.Code != 409 || !strings.Contains(issue.Body.String(), "source is archived") {
		t.Fatalf("issue after archive = %d %s", issue.Code, issue.Body.String())
	}
	credential := f.store.seedCredential(f.workspaceID, sid, nil)
	if w := f.do(t, http.MethodPost, f.rotatePath(credential.ID.String()), ""); w.Code != 409 {
		t.Fatalf("rotate after archive = %d", w.Code)
	}
	// List and revoke on an archived source still work.
	if w := f.do(t, http.MethodGet, f.wsPath(f.workspaceID, "/sources/"+created.SourceID+"/credentials"), ""); w.Code != 200 {
		t.Fatalf("credential list on archived source = %d", w.Code)
	}
	if w := f.do(t, http.MethodPost, f.revokePath(credential.ID.String()), ""); w.Code != 204 {
		t.Fatalf("revoke on archived source = %d", w.Code)
	}
	list := f.do(t, http.MethodGet, listPath, "")
	var items []sourceBody
	if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0].ArchivedAt == nil {
		t.Fatalf("archived source must stay listed: %s", list.Body.String())
	}
}

func TestManagementRoutesFailClosed(t *testing.T) {
	adapter, _ := newTestAuth(t)
	memberships := workspaces.New(newRoleFakeStore())
	projectService := projects.New(&projectFakeStore{})
	sourceService := sources.New(&sourceFakeStore{})
	cases := map[string]Options{
		"no auth":            {Projects: NewProjects(projectService, memberships), Sources: NewSources(sourceService, memberships)},
		"no memberships":     {Auth: adapter, Projects: NewProjects(projectService, nil), Sources: NewSources(sourceService, nil)},
		"no services":        {Auth: adapter, Projects: NewProjects(nil, memberships), Sources: NewSources(nil, memberships)},
		"nothing configured": {Auth: adapter},
	}
	paths := []string{"/api/v1/workspaces/" + idWithByte(1).String() + "/projects", "/api/v1/workspaces/" + idWithByte(1).String() + "/sources"}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			handler := NewHandler(options)
			for _, path := range paths {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
					if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
						t.Fatalf("%s %s = %d, want route absent", method, path, w.Code)
					}
				}
			}
		})
	}
}
