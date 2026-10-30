package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/dundunHa/TokenPivot/internal/usage"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
)

// Usage bundles the read-only aggregate query dependencies: the query service and
// the membership lookup that gates every request. The session check is the shared
// Auth middleware, so this adapter never parses a cookie itself.
type Usage struct {
	Service    *usage.Service
	Workspaces *workspaces.Service
}

// Aggregate query routes. Both are browser GET endpoints under /api/v1.
const (
	usageAggregatePath = "/api/v1/workspaces/{workspaceID}/usage/aggregate"
	usageRankingsPath  = "/api/v1/workspaces/{workspaceID}/usage/rankings"
	usageOptionsPath   = "/api/v1/workspaces/{workspaceID}/usage/options"
)

// usageErrorEnvelope is the error body of the query endpoints: the shared code and
// message plus optional machine-readable details.
type usageErrorEnvelope struct {
	Error usageErrorBody `json:"error"`
}

type usageErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type outsideRetentionDetails struct {
	RetentionCutoff string `json:"retention_cutoff"`
}

type tooManyPointsDetails struct {
	MaxPoints            int                `json:"max_points"`
	SuggestedGranularity *usage.Granularity `json:"suggested_granularity"`
}

// NewUsage returns the query adapter for a usage service and a membership service.
// Both are required; routes are only registered when the handler also has an Auth
// adapter for the session gate.
func NewUsage(service *usage.Service, memberships *workspaces.Service) *Usage {
	return &Usage{Service: service, Workspaces: memberships}
}

// routes registers the two GET endpoints. Every response, including session
// failures, is uncacheable. A missing service, membership lookup or session
// adapter registers nothing, so the paths stay unreachable instead of failing open.
func (u *Usage) routes(mux *http.ServeMux, sessions *Auth) {
	if u == nil || u.Service == nil || u.Workspaces == nil || sessions == nil || sessions.Service == nil {
		return
	}
	mux.Handle("GET "+usageAggregatePath, sessions.readRoute(http.HandlerFunc(u.handleAggregate)))
	mux.Handle("GET "+usageRankingsPath, sessions.readRoute(http.HandlerFunc(u.handleRankings)))
	mux.Handle("GET "+usageOptionsPath, sessions.readRoute(http.HandlerFunc(u.handleOptions)))
}

func (u *Usage) handleAggregate(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, u.Workspaces)
	if !ok {
		return
	}
	values, ok := usageQuery(w, r)
	if !ok {
		return
	}
	response, err := u.Service.Aggregate(r.Context(), workspaceID, values)
	if err != nil {
		writeUsageError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (u *Usage) handleRankings(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, u.Workspaces)
	if !ok {
		return
	}
	values, ok := usageQuery(w, r)
	if !ok {
		return
	}
	response, err := u.Service.Rankings(r.Context(), workspaceID, values)
	if err != nil {
		writeUsageError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleOptions lists the provider and model filter options. It takes no query
// parameters: any query string is refused so a typo cannot look like a filter.
func (u *Usage) handleOptions(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := requireMember(w, r, u.Workspaces)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeUsageError(r, w, usage.ErrInvalidArgument)
		return
	}
	response, err := u.Service.Options(r.Context(), workspaceID)
	if err != nil {
		writeUsageError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// usageQuery parses the raw query string strictly: malformed escapes and
// semicolons are refused instead of being dropped, as r.URL.Query would.
func usageQuery(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeUsageError(r, w, usage.ErrInvalidArgument)
		return nil, false
	}
	return values, true
}

// writeUsageError maps a query error to its status and code. Messages are fixed
// text: no request value, SQL or database message is ever repeated.
func writeUsageError(r *http.Request, w http.ResponseWriter, err error) {
	var outside *usage.OutsideRetentionError
	var tooMany *usage.TooManyPointsError
	switch {
	case errors.Is(err, usage.ErrInvalidArgument):
		writeJSON(w, http.StatusBadRequest, usageErrorEnvelope{Error: usageErrorBody{Code: "INVALID_ARGUMENT", Message: "request is not acceptable"}})
	case errors.As(err, &outside):
		writeJSON(w, http.StatusBadRequest, usageErrorEnvelope{Error: usageErrorBody{
			Code: "OUTSIDE_RETENTION", Message: "start is before the retention cutoff",
			Details: outsideRetentionDetails{RetentionCutoff: outside.Cutoff.UTC().Format(time.RFC3339Nano)},
		}})
	case errors.As(err, &tooMany):
		message := "time range has too many points"
		if tooMany.Suggested == nil {
			message = "time range has too many points; shorten the time range"
		}
		writeJSON(w, http.StatusBadRequest, usageErrorEnvelope{Error: usageErrorBody{
			Code: "TOO_MANY_POINTS", Message: message,
			Details: tooManyPointsDetails{MaxPoints: tooMany.MaxPoints, SuggestedGranularity: tooMany.Suggested},
		}})
	default:
		slog.ErrorContext(r.Context(), "usage request failed", "stage", "usage", "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}
