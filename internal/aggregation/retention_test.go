package aggregation

import (
	"testing"
	"time"
)

func utc(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRetentionCutoffCases(t *testing.T) {
	tests := []struct{ name, now, want string }{
		{"ordinary day keeps time and zeroes seconds", "2026-10-28T01:30:45.123Z", "2025-09-28T01:30:00Z"},
		{"year boundary", "2026-01-15T10:00:00Z", "2024-12-15T10:00:00Z"},
		{"december 31 overflows november", "2026-12-31T23:59:59Z", "2025-12-01T00:00:00Z"},
		{"december 30 fits november", "2026-12-30T23:59:59Z", "2025-11-30T23:59:00Z"},
		{"month end overflow to march first", "2027-03-31T12:00:00Z", "2026-03-01T00:00:00Z"},
		{"last kept day before overflow", "2027-03-28T23:59:59Z", "2026-02-28T23:59:00Z"},
		{"first overflow day", "2027-03-29T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"leap target february 29 exists", "2029-03-29T08:15:00Z", "2028-02-29T08:15:00Z"},
		{"leap target overflow day 30", "2029-03-30T08:15:00Z", "2028-03-01T00:00:00Z"},
		{"2029-03-31 maps to 2028-03-01", "2029-03-31T00:00:30Z", "2028-03-01T00:00:00Z"},
		{"thirty day target month overflow july 31", "2026-07-31T05:00:00Z", "2025-07-01T00:00:00Z"},
		{"thirty day target month fits", "2026-07-30T05:00:00Z", "2025-06-30T05:00:00Z"},
		{"thirty one day target month", "2026-08-31T05:00:00Z", "2025-07-31T05:00:00Z"},
		{"non UTC input is converted first", "2026-10-28T09:30:00+08:00", "2025-09-28T01:30:00Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := RetentionCutoff(utc(test.now)); !got.Equal(utc(test.want)) || got.Location() != time.UTC {
				t.Fatalf("RetentionCutoff(%s) = %s, want %s", test.now, got.Format(time.RFC3339Nano), test.want)
			}
		})
	}
}

// TestRetentionCutoffNeverDecreases walks four years minute by minute across the
// short-month overflows and proves a later now never yields an earlier cutoff.
func TestRetentionCutoffNeverDecreases(t *testing.T) {
	now := utc("2027-02-20T00:00:00Z")
	end := utc("2029-05-01T00:00:00Z")
	previous := RetentionCutoff(now)
	for now = now.Add(7 * time.Minute); now.Before(end); now = now.Add(7 * time.Minute) {
		got := RetentionCutoff(now)
		if got.Before(previous) {
			t.Fatalf("cutoff moved back at %s: %s < %s", now.Format(time.RFC3339), got.Format(time.RFC3339), previous.Format(time.RFC3339))
		}
		if got.Second() != 0 || got.Nanosecond() != 0 {
			t.Fatalf("cutoff %s is not minute aligned", got.Format(time.RFC3339Nano))
		}
		previous = got
	}
}

func TestRetentionCutoffBoundaryEquality(t *testing.T) {
	now := utc("2026-10-28T01:30:00Z")
	cutoff := RetentionCutoff(now)
	if cutoff.Before(cutoff) || !cutoff.Add(-time.Minute).Before(cutoff) {
		t.Fatal("a start equal to the cutoff must be allowed and one minute earlier rejected")
	}
}
