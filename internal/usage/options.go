package usage

import (
	"context"
	"sort"
	"time"

	"github.com/dundunHa/TokenPivot/internal/aggregation"
	"github.com/jackc/pgx/v5/pgtype"
)

// OptionsParams selects the minute rows whose provider and model values are
// offered as filter options: one workspace and the half-open range [Start,End).
type OptionsParams struct {
	WorkspaceID pgtype.UUID
	Start, End  time.Time
}

// OptionsRows is the store answer: the distinct provider and model values found.
type OptionsRows struct {
	Providers []string
	Models    []string
}

// OptionsResponse is the filter option list. Provider and Model are independent
// dimensions; both slices are always non-nil so an empty result encodes as [].
type OptionsResponse struct {
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
}

// Options lists the provider and model values that occur in the workspace's
// minute statistics inside the retention window. The clock is sampled once: the
// window is [RetentionCutoff(as_of), as_of), so it never depends on physical
// cleanup. Values are merged by exact equality and sorted by byte order, so the
// result is stable; nothing is truncated.
func (s *Service) Options(ctx context.Context, workspaceID pgtype.UUID) (*OptionsResponse, error) {
	now := s.sample()
	rows, err := s.store.Options(ctx, OptionsParams{
		WorkspaceID: workspaceID, Start: aggregation.RetentionCutoff(now), End: now,
	})
	if err != nil {
		return nil, err
	}
	return &OptionsResponse{Providers: normalizeOptions(rows.Providers), Models: normalizeOptions(rows.Models)}, nil
}

// normalizeOptions merges equal values and sorts them in byte order, matching the
// C collation used by the store.
func normalizeOptions(list []string) []string {
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, v := range list {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
