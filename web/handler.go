// Package web serves the optional embedded frontend without runtime disk access.
package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

func newHandler(fsys fs.FS) (http.Handler, error) {
	index, info, err := readFile(fsys, "index.html")
	if err != nil {
		return nil, fmt.Errorf("frontend entry: %w", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		for _, segment := range strings.Split(name, "/") {
			if strings.HasPrefix(segment, ".") || strings.Contains(segment, "\\") {
				http.NotFound(w, r)
				return
			}
		}
		if name == "" || name == "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeContent(w, r, "index.html", info.ModTime(), bytes.NewReader(index))
			return
		}
		name = strings.TrimSuffix(name, "/")
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		data, fileInfo, err := readFile(fsys, name)
		if err == nil {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeContent(w, r, name, fileInfo.ModTime(), bytes.NewReader(data))
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			if errors.Is(err, errNotRegular) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}
		if name != "assets" && !strings.HasPrefix(name, "assets/") && path.Ext(name) == "" && acceptsHTML(r.Header.Values("Accept")) {
			w.Header().Set("Cache-Control", "no-cache")
			http.ServeContent(w, r, "index.html", info.ModTime(), bytes.NewReader(index))
			return
		}
		http.NotFound(w, r)
	}), nil
}

var errNotRegular = errors.New("not a regular file")

func readFile(fsys fs.FS, name string) ([]byte, fs.FileInfo, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.Join(errNotRegular, file.Close())
	}
	data, err := io.ReadAll(file)
	err = errors.Join(err, file.Close())
	return data, info, err
}

func acceptsHTML(headers []string) bool {
	for _, header := range headers {
		for _, item := range strings.Split(header, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || mediaType != "text/html" {
				continue
			}
			quality := 1.0
			if value, ok := params["q"]; ok {
				quality, err = strconv.ParseFloat(value, 64)
			}
			if err == nil && quality > 0 && quality <= 1 {
				return true
			}
		}
	}
	return false
}
