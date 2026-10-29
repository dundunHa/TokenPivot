package aggregation

import "time"

// RetentionMonths is the number of calendar months minute statistics stay
// queryable (ADR-0003).
const RetentionMonths = 13

// RetentionCutoff returns the oldest minute start still inside retention at now.
// It is computed in UTC only: the year and month move RetentionMonths into the
// past, the day, hour and minute are kept with the seconds zeroed. When the
// original day does not exist in the target month the cutoff is the first day
// of the following month at 00:00 UTC. Rows with bucket_start < cutoff may be
// cleaned; a row equal to the cutoff is kept.
//
// The result never decreases as now increases. Clamping to the last day of a
// short month and then reusing the time of day would let it move backwards and
// reopen an already cleaned range; time.AddDate would normalise the overflow
// differently and is deliberately not used.
func RetentionCutoff(now time.Time) time.Time {
	u := now.UTC()
	year, month := u.Year(), int(u.Month())-RetentionMonths
	for month < 1 {
		month += 12
		year--
	}
	target := time.Month(month)
	// Day 0 of the next month is the last day of the target month.
	if u.Day() <= time.Date(year, target+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return time.Date(year, target, u.Day(), u.Hour(), u.Minute(), 0, 0, time.UTC)
	}
	return time.Date(year, target+1, 1, 0, 0, 0, 0, time.UTC)
}
