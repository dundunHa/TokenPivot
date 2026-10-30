package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/dundunHa/TokenPivot/internal/requestdetails"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// RequestDetails bundles the read-only request detail and rate dependencies. The
// session check is the shared Auth middleware.
type RequestDetails struct {
	Service    *requestdetails.Service
	Workspaces *workspaces.Service
}

// Request detail routes under /api/v1.
const (
	usageEventsPath = "/api/v1/workspaces/{workspaceID}/usage-events"
	usageRatePath   = "/api/v1/workspaces/{workspaceID}/usage-rate"
)

// NewRequestDetails returns the adapter; both dependencies are required.
func NewRequestDetails(service *requestdetails.Service, memberships *workspaces.Service) *RequestDetails {
	return &RequestDetails{Service: service, Workspaces: memberships}
}

// routes registers the two GET endpoints. A missing dependency registers
// nothing, so the paths stay unreachable instead of failing open.
func (d *RequestDetails) routes(mux *http.ServeMux, sessions *Auth) {
	if d == nil || d.Service == nil || d.Workspaces == nil || sessions == nil || sessions.Service == nil {
		return
	}
	mux.Handle("GET "+usageEventsPath, sessions.readRoute(http.HandlerFunc(d.handleEvents)))
	mux.Handle("GET "+usageRatePath, sessions.readRoute(http.HandlerFunc(d.handleRate)))
}

func (d *RequestDetails) handleEvents(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, d.Workspaces)
	if !ok {
		return
	}
	values, ok := requestDetailsQuery(w, r)
	if !ok {
		return
	}
	response, err := d.Service.Events(r.Context(), workspaceID, values)
	if err != nil {
		writeRequestDetailsError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (d *RequestDetails) handleRate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, d.Workspaces)
	if !ok {
		return
	}
	values, ok := requestDetailsQuery(w, r)
	if !ok {
		return
	}
	response, err := d.Service.Rate(r.Context(), workspaceID, values)
	if err != nil {
		writeRequestDetailsError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// requestDetailsQuery parses the raw query strictly, refusing malformed escapes.
func requestDetailsQuery(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeRequestDetailsError(r, w, requestdetails.ErrInvalidArgument)
		return nil, false
	}
	return values, true
}

// writeRequestDetailsError maps an error to a fixed message: no request value,
// SQL or database message is repeated. A cancelled or unavailable database is
// 503.
func writeRequestDetailsError(r *http.Request, w http.ResponseWriter, err error) {
	if errors.Is(err, requestdetails.ErrInvalidArgument) {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request is not acceptable")
		return
	}
	slog.ErrorContext(r.Context(), "request details failed", "stage", "requestdetails", "err", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}
