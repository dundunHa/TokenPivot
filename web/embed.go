//go:build uiembed

package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
)

//go:embed dist
var assets embed.FS

// Handler serves the frontend embedded at build time, validating its entry point.
func Handler() (http.Handler, error) {
	root, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil, fmt.Errorf("frontend filesystem: %w", err)
	}
	return newHandler(root)
}
