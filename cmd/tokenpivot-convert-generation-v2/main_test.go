package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// discardLogger keeps unit output silent while still exercising the code paths that
// log a report.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		apply   bool
		wantErr error
	}{
		{name: "no argument is a check", args: nil},
		{name: "apply", args: []string{"--apply"}, apply: true},
		{name: "help", args: []string{"--help"}, wantErr: errHelp},
		{name: "short help", args: []string{"-h"}, wantErr: errHelp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			apply, err := parseArgs(tc.args)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if apply != tc.apply {
				t.Fatalf("apply = %v, want %v", apply, tc.apply)
			}
		})
	}
}

func TestParseArgsRefusesEveryOtherInvocation(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--apply", "extra"},
		{"--dsn", "postgres://hidden"},
		{"apply"},
		{"--check"},
	} {
		if _, err := parseArgs(args); err == nil || errors.Is(err, errHelp) {
			t.Fatalf("args %v produced err %v, want a usage error", args, err)
		}
	}
}

func TestRunRejectsArgumentsBeforeTouchingTheDatabase(t *testing.T) {
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "")
	err := run(context.Background(), []string{"--nope"}, discardLogger())
	if err == nil {
		t.Fatal("run accepted an unknown argument")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("err = %v, want the usage line", err)
	}
}

func TestRunRequiresTheMigrationConnection(t *testing.T) {
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "   ")
	err := run(context.Background(), nil, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "TOKENPIVOT_MIGRATION_DATABASE_URL is required") {
		t.Fatalf("err = %v, want the required variable error", err)
	}
}

func TestRunNeverEchoesACredential(t *testing.T) {
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "postgres://owner:super-secret-value@127.0.0.1:1/tokenpivot_test?sslmode=disable")
	err := run(context.Background(), nil, discardLogger())
	if err == nil {
		t.Fatal("run reported success against an unreachable server")
	}
	if strings.Contains(err.Error(), "super-secret-value") || strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("err = %v, want no connection detail", err)
	}
}

func TestCheckState(t *testing.T) {
	t.Parallel()
	if got := checkState(state{V1Rows: 0, V2Rows: 4}); got != "converted" {
		t.Fatalf("state = %q, want converted", got)
	}
	if got := checkState(state{V1Rows: 3}); got != "needs_conversion" {
		t.Fatalf("state = %q, want needs_conversion", got)
	}
}

func TestStoredMetricRejectsEveryPairTheApiCannotProduce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   int64
		has     bool
		quality string
		wantErr bool
	}{
		{name: "known with a value", value: 7, has: true, quality: "known"},
		{name: "known with a real zero", value: 0, has: true, quality: "known"},
		{name: "estimated with a real zero", value: 0, has: true, quality: "estimated"},
		{name: "estimated at the safe ceiling", value: 9007199254740991, has: true, quality: "estimated"},
		{name: "missing without a value", quality: "missing"},
		{name: "known without a value", quality: "known", wantErr: true},
		{name: "estimated without a value", quality: "estimated", wantErr: true},
		{name: "missing with a value", value: 7, has: true, quality: "missing", wantErr: true},
		{name: "negative value", value: -1, has: true, quality: "known", wantErr: true},
		{name: "value beyond the safe ceiling", value: 9007199254740992, has: true, quality: "estimated", wantErr: true},
		{name: "unknown quality", value: 7, has: true, quality: "guessed", wantErr: true},
		{name: "absent quality", value: 7, has: true, quality: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metric, err := storedMetric(sql.NullInt64{Int64: tc.value, Valid: tc.has}, sql.NullString{String: tc.quality, Valid: tc.quality != ""})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("pair (%d,%v,%q) was accepted", tc.value, tc.has, tc.quality)
				}
				return
			}
			if err != nil {
				t.Fatalf("pair (%d,%v,%q): %v", tc.value, tc.has, tc.quality, err)
			}
			if string(metric.Quality) != tc.quality {
				t.Fatalf("quality = %q, want %q", metric.Quality, tc.quality)
			}
			if tc.has != (metric.Value != nil) {
				t.Fatalf("value = %v, want present %v", metric.Value, tc.has)
			}
			if tc.has && *metric.Value != tc.value {
				t.Fatalf("value = %d, want %d", *metric.Value, tc.value)
			}
		})
	}
}

func TestStoredQualityRejectsEverythingItDoesNotKnow(t *testing.T) {
	t.Parallel()
	if _, err := storedQuality(nullString("")); err == nil {
		t.Fatal("a missing quality was accepted")
	}
	if _, err := storedQuality(nullString("guessed")); err == nil {
		t.Fatal("an unknown quality was accepted")
	}
	for _, value := range []string{"known", "estimated", "missing"} {
		quality, err := storedQuality(nullString(value))
		if err != nil {
			t.Fatalf("quality %q: %v", value, err)
		}
		if string(quality) != value {
			t.Fatalf("quality = %q, want %q", quality, value)
		}
	}
}

// nullString builds the scanned form of a stored quality, where the empty value
// stands for a missing column rather than an empty string.
func nullString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func TestUUIDTextRendersTheHyphenatedForm(t *testing.T) {
	t.Parallel()
	id := pgtype.UUID{Bytes: [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, Valid: true}
	if got := uuidText(id); got != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("uuidText = %q", got)
	}
}

func TestCommitUnknownErrorIsDistinguishable(t *testing.T) {
	t.Parallel()
	cause := errors.New("transport closed")
	err := error(&commitUnknownError{cause: cause})
	var unknown *commitUnknownError
	if !errors.As(err, &unknown) {
		t.Fatal("the commit unknown error is not recognized")
	}
	if !errors.Is(err, cause) {
		t.Fatal("the underlying cause is not reachable")
	}
	if !strings.Contains(err.Error(), "commit result unknown") {
		t.Fatalf("err = %q, want the unknown status", err)
	}
}
