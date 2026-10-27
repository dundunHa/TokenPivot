package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestArgumentsBeforeConfig(t *testing.T) {
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "synthetic-sensitive-marker")
	for _, args := range [][]string{nil, {"reset"}, {"force"}, {"synthetic-sensitive-marker"}, {"up", "extra"}} {
		var out bytes.Buffer
		err := run(context.Background(), args, slog.New(slog.NewJSONHandler(&out, nil)))
		if err == nil || !strings.Contains(err.Error(), "usage:") || strings.Contains(err.Error(), "synthetic-sensitive-marker") || out.Len() != 0 {
			t.Fatal("unsafe argument handling")
		}
	}
}

func TestMigrationMissingConfig(t *testing.T) {
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "")
	t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
	var out bytes.Buffer
	err := run(context.Background(), []string{"up"}, slog.New(slog.NewJSONHandler(&out, nil)))
	if err == nil || !strings.Contains(err.Error(), "TOKENPIVOT_MIGRATION_DATABASE_URL") || strings.Contains(err.Error(), "synthetic-sensitive-marker") {
		t.Fatal("migration config fallback or leak")
	}
}
