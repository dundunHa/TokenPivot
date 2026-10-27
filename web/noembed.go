//go:build !uiembed

package web

import "net/http"

// Handler returns nil for backend-only builds; no frontend files are required.
func Handler() (http.Handler, error) {
	return nil, nil
}
