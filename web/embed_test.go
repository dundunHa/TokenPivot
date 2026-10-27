//go:build uiembed

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedAssets(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	get := func(path string) (string, string) {
		t.Helper()
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s missing no-cache", path)
		}
		return string(body), resp.Header.Get("Content-Type")
	}
	index, contentType := get("/")
	if !strings.Contains(index, "TokenPivot") || !strings.Contains(contentType, "text/html") {
		t.Fatal("missing real entry")
	}
	refs := regexp.MustCompile(`(?:src|href)="(/assets/[^\"]+)"`).FindAllStringSubmatch(index, -1)
	var js, css bool
	for _, ref := range refs {
		body, contentType := get(ref[1])
		if body == "" {
			t.Errorf("empty asset %s", ref[1])
		}
		if strings.HasSuffix(ref[1], ".js") {
			js = true
			if !strings.Contains(contentType, "javascript") {
				t.Error("incorrect JS MIME")
			}
		}
		if strings.HasSuffix(ref[1], ".css") {
			css = true
			if !strings.Contains(contentType, "text/css") {
				t.Error("incorrect CSS MIME")
			}
		}
		if strings.Contains(body, "TOKENPIVOT_DEV_API_TARGET") || strings.Contains(body, "http://127.0.0.1:8080") {
			t.Error("development target leaked into asset")
		}
	}
	if !js || !css {
		t.Fatal("entry must reference built JS and CSS")
	}
	if deep, _ := get("/index.html"); deep != index {
		t.Error("direct entry differs")
	}
}
