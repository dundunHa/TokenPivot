//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
)

const (
	sourceArchivalVersion = 7
	beforeArchival        = 6
)

// TestSourceArchivalMigration pins itself with UpTo/DownTo so later migrations
// cannot change these assertions.
func TestSourceArchivalMigration(t *testing.T) {
	migrationURL, _ := integrationSchema(t)
	p, db := testProvider(t, migrationURL, migrations.Files)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := p.UpTo(ctx, beforeArchival); err != nil {
		t.Fatal(database.SafeError("up to 6", err))
	}
	var workspaceID, sourceID string
	if err := db.QueryRowContext(ctx, "INSERT INTO workspaces (name) VALUES ('synthetic') RETURNING id::text").Scan(&workspaceID); err != nil {
		t.Fatal(database.SafeError("seed workspace", err))
	}
	if err := db.QueryRowContext(ctx, "INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1,'dsh','old') RETURNING id::text", workspaceID).Scan(&sourceID); err != nil {
		t.Fatal(database.SafeError("seed old source", err))
	}
	if _, err := p.UpTo(ctx, sourceArchivalVersion); err != nil {
		t.Fatal(database.SafeError("up to 7", err))
	}
	version, err := p.GetDBVersion(ctx)
	if err != nil || version != sourceArchivalVersion {
		t.Fatalf("version=%d err=%v want %d", version, err, sourceArchivalVersion)
	}
	var dataType, nullable, defaultText string
	if err := db.QueryRowContext(ctx, "SELECT data_type::text, is_nullable::text, coalesce(column_default,'') FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='sources' AND column_name='archived_at'").Scan(&dataType, &nullable, &defaultText); err != nil {
		t.Fatal(database.SafeError("column", err))
	}
	if dataType != "timestamp with time zone" || nullable != "YES" || defaultText != "" {
		t.Fatalf("archived_at type=%q nullable=%q default=%q", dataType, nullable, defaultText)
	}
	var archived bool
	if err := db.QueryRowContext(ctx, "SELECT archived_at IS NOT NULL FROM sources WHERE id=$1", sourceID).Scan(&archived); err != nil || archived {
		t.Fatalf("old source must stay active: archived=%v err=%v", archived, err)
	}
	var columns int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name='archived_at'").Scan(&columns); err != nil || columns != 2 {
		t.Fatalf("archived_at columns=%d err=%v want projects+sources", columns, err)
	}
	if _, err := p.DownTo(ctx, beforeArchival); err != nil {
		t.Fatal(database.SafeError("down to 6", err))
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='sources' AND column_name='archived_at'").Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("down left the column: %d err=%v", columns, err)
	}
}
