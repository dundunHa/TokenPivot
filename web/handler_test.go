package web

import (
	"errors"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const testIndex = "<!doctype html><title>test entry</title>"

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte(testIndex)},
		"assets/app-123.js":  {Data: []byte("console.log('test')")},
		"assets/app-123.css": {Data: []byte("body{color:black}")},
		"docs/index.html":    {Data: []byte("private directory index")},
		".hidden":            {Data: []byte("hidden")},
		"docs/.hidden":       {Data: []byte("hidden")},
	}
}

func TestStaticHandler(t *testing.T) {
	h, err := newHandler(testFS())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		method, target, accept string
		status                 int
		body, contentType      string
	}{
		{"GET", "/", "", 200, testIndex, "text/html"},
		{"GET", "/index.html", "", 200, testIndex, "text/html"},
		{"HEAD", "/index.html", "", 200, "", "text/html"},
		{"GET", "/assets/app-123.js", "", 200, "console.log('test')", "javascript"},
		{"GET", "/assets/app-123.css", "", 200, "body{color:black}", "text/css"},
		{"HEAD", "/assets/app-123.js", "", 200, "", "javascript"},
		{"GET", "/workspace/one", "text/html", 200, testIndex, "text/html"},
		{"HEAD", "/workspace/one", "text/html", 200, "", "text/html"},
		{"GET", "/workspace/one/", "application/json, text/html;q=0.2", 200, testIndex, "text/html"},
		{"GET", "/workspace/one", "text/html;q=0", 404, "", ""},
		{"GET", "/workspace/one", "text/html;q=invalid", 404, "", ""},
		{"GET", "/workspace/one", "text/html;q=NaN", 404, "", ""},
		{"GET", "/workspace/one", "text/html;q=1.1", 404, "", ""},
		{"GET", "/workspace/one", "*/*", 404, "", ""},
		{"GET", "/workspace/one", "application/json", 404, "", ""},
		{"GET", "/workspace/one", "", 404, "", ""},
		{"GET", "/missing.js", "text/html", 404, "", ""},
		{"GET", "/missing.css", "text/html", 404, "", ""},
		{"GET", "/missing.png", "text/html", 404, "", ""},
		{"GET", "/favicon.ico", "text/html", 404, "", ""},
		{"GET", "/missing.js.map", "text/html", 404, "", ""},
		{"GET", "/assets", "text/html", 404, "", ""},
		{"GET", "/assets/", "text/html", 404, "", ""},
		{"GET", "/assets/missing", "text/html", 404, "", ""},
		{"GET", "/docs", "text/html", 404, "", ""},
		{"GET", "/docs/", "text/html", 404, "", ""},
		{"GET", "/.hidden", "text/html", 404, "", ""},
		{"GET", "/docs/.hidden", "text/html", 404, "", ""},
		{"GET", "/docs/../index.html", "text/html", 404, "", ""},
		{"GET", "/%2e%2e/index.html", "text/html", 404, "", ""},
		{"GET", "/docs/%2ehidden", "text/html", 404, "", ""},
		{"GET", "/docs%5c..%5cindex.html", "text/html", 404, "", ""},
		{"POST", "/", "text/html", 405, "", ""},
		{"PUT", "/assets/app-123.js", "text/html", 405, "", ""},
		{"OPTIONS", "/route", "text/html", 405, "", ""},
	} {
		t.Run(tt.method+tt.target+tt.accept, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.target, nil)
			r.Header.Set("Accept", tt.accept)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if tt.status == 200 {
				if w.Body.String() != tt.body {
					t.Errorf("body = %q", w.Body.String())
				}
				if !strings.Contains(w.Header().Get("Content-Type"), tt.contentType) {
					t.Errorf("content type = %q", w.Header().Get("Content-Type"))
				}
				if w.Header().Get("Cache-Control") != "no-cache" {
					t.Error("missing no-cache")
				}
			} else if strings.Contains(w.Body.String(), testIndex) {
				t.Error("error returned entry HTML")
			}
			if tt.status == 405 && w.Header().Get("Allow") != "GET, HEAD" {
				t.Error("missing Allow")
			}
		})
	}
}

// faultyFS injects real IO errors, including failures after a successful Open.
type faultyFS struct {
	fs.FS
	name, stage string
}

func (f faultyFS) Open(name string) (fs.File, error) {
	if name != f.name {
		return f.FS.Open(name)
	}
	if f.stage == "open" {
		return nil, fs.ErrPermission
	}
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return faultyFile{File: file, stage: f.stage}, nil
}

type faultyFile struct {
	fs.File
	stage string
}

func (f faultyFile) Read(p []byte) (int, error) {
	if f.stage == "read" {
		return 0, errors.New("internal read failure")
	}
	return f.File.Read(p)
}
func (f faultyFile) Stat() (fs.FileInfo, error) {
	if f.stage == "stat" {
		return nil, errors.New("internal stat failure")
	}
	return f.File.Stat()
}
func (f faultyFile) Close() error {
	err := f.File.Close()
	if f.stage == "close" {
		return errors.Join(err, errors.New("internal close failure"))
	}
	return err
}

func TestInitializationFailures(t *testing.T) {
	for _, fsys := range []fs.FS{fstest.MapFS{}, fstest.MapFS{"index.html/child": {Data: []byte("not an entry")}}, fstest.MapFS{"index.html": {Mode: fs.ModeSymlink}}} {
		if _, err := newHandler(fsys); err == nil {
			t.Error("invalid index accepted")
		}
	}
	for _, stage := range []string{"open", "stat", "read", "close"} {
		if _, err := newHandler(faultyFS{FS: testFS(), name: "index.html", stage: stage}); err == nil {
			t.Errorf("%s failure accepted", stage)
		}
	}
}

func TestReadFailuresDoNotFallback(t *testing.T) {
	for _, stage := range []string{"open", "stat", "read", "close"} {
		t.Run(stage, func(t *testing.T) {
			files := testFS()
			files["broken"] = &fstest.MapFile{Data: []byte("not served")}
			h, err := newHandler(faultyFS{FS: files, name: "broken", stage: stage})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/broken", nil)
			r.Header.Set("Accept", "text/html")
			h.ServeHTTP(w, r)
			if w.Code != 500 || w.Body.String() != "Internal Server Error\n" {
				t.Fatalf("response = %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestDecodedPathIsNotDecodedTwice(t *testing.T) {
	files := testFS()
	files["%2ehidden"] = &fstest.MapFile{Data: []byte("literal percent filename")}
	h, err := newHandler(files)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/%252ehidden", nil))
	if w.Code != 200 || w.Body.String() != "literal percent filename" {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
}
