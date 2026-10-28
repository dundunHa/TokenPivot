//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
)

const (
	usageMinuteStatsVersion = 4
	authTablesVersion       = 5
)

var authTables = []string{"users", "sessions"}

// authColumn is the expected shape of one information_schema.columns row.
type authColumn struct {
	dataType    string
	nullable    string
	defaultText string
	collation   string
}

func expectedAuthColumns() map[string]authColumn {
	id := authColumn{dataType: "uuid", nullable: "NO", defaultText: "uuidv7()"}
	createdAt := authColumn{dataType: "timestamp with time zone", nullable: "NO", defaultText: "now()"}
	return map[string]authColumn{
		"users.id":            id,
		"users.email":         {dataType: "text", nullable: "NO", collation: "C"},
		"users.password_hash": {dataType: "text", nullable: "NO"},
		"users.created_at":    createdAt,
		"sessions.id":         id,
		"sessions.user_id":    {dataType: "uuid", nullable: "NO"},
		"sessions.token_hash": {dataType: "bytea", nullable: "NO"},
		// expires_at deliberately has no default: every writer states it.
		"sessions.expires_at": {dataType: "timestamp with time zone", nullable: "NO"},
		"sessions.revoked_at": {dataType: "timestamp with time zone", nullable: "YES"},
		"sessions.created_at": createdAt,
	}
}

// authIndex is the expected shape of one pg_index row.
type authIndex struct {
	unique  bool
	columns string
}

func expectedAuthIndexes() map[string]authIndex {
	return map[string]authIndex{
		"users_pkey":             {unique: true, columns: "id"},
		"uk_users_email":         {unique: true, columns: "email"},
		"sessions_pkey":          {unique: true, columns: "id"},
		"uk_sessions_token_hash": {unique: true, columns: "token_hash"},
		"idx_sessions_user_id":   {unique: false, columns: "user_id"},
	}
}

// TestAuthTables pins itself to migration 00005 with UpTo/DownTo so later
// migrations cannot change these assertions.
func TestAuthTables(t *testing.T) {
	migrationURL, appURL := integrationSchema(t)
	p, db := testProvider(t, migrationURL, migrations.Files)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := p.UpTo(ctx, authTablesVersion); err != nil {
		t.Fatal(database.SafeError("auth up", err))
	}
	app, err := pgx.Connect(ctx, appURL)
	if err != nil {
		t.Fatal(database.SafeError("app connect", err))
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error("close app failed")
		}
	}()

	t.Run("structure", func(t *testing.T) {
		query := "SELECT table_name::text, column_name::text, data_type::text, is_nullable::text, coalesce(column_default,'')::text, coalesce(collation_name,'')::text FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('users','sessions')"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("columns", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close column rows failed")
			}
		}()
		actual := make(map[string]authColumn)
		for rows.Next() {
			var table, column string
			var spec authColumn
			if err := rows.Scan(&table, &column, &spec.dataType, &spec.nullable, &spec.defaultText, &spec.collation); err != nil {
				t.Fatal(database.SafeError("scan column", err))
			}
			actual[table+"."+column] = spec
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("column rows", err))
		}
		expected := expectedAuthColumns()
		if len(actual) != len(expected) {
			t.Fatalf("column count=%d want=%d", len(actual), len(expected))
		}
		for name, want := range expected {
			got, ok := actual[name]
			if !ok {
				t.Fatalf("missing column %s", name)
			}
			if got.dataType != want.dataType || got.nullable != want.nullable || got.collation != want.collation {
				t.Fatalf("column %s type=%q nullable=%q collation=%q want type=%q nullable=%q collation=%q", name, got.dataType, got.nullable, got.collation, want.dataType, want.nullable, want.collation)
			}
			if want.defaultText == "" {
				if got.defaultText != "" {
					t.Fatalf("column %s has unexpected default %q", name, got.defaultText)
				}
				continue
			}
			if !strings.Contains(got.defaultText, want.defaultText) {
				t.Fatalf("column %s default=%q want contains %q", name, got.defaultText, want.defaultText)
			}
		}
		t.Logf("auth columns=%d all match", len(actual))
	})

	t.Run("indexes", func(t *testing.T) {
		query := "SELECT c.relname::text, i.relname::text, ix.indisunique, (SELECT string_agg(a.attname, ',' ORDER BY u.k) FROM unnest(string_to_array(ix.indkey::text, ' ')::int[]) WITH ORDINALITY AS u(attnum, k) JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=u.attnum)::text FROM pg_index ix JOIN pg_class i ON i.oid=ix.indexrelid JOIN pg_class c ON c.oid=ix.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('users','sessions')"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("indexes", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close index rows failed")
			}
		}()
		actual := make(map[string]authIndex)
		for rows.Next() {
			var table, name string
			var spec authIndex
			if err := rows.Scan(&table, &name, &spec.unique, &spec.columns); err != nil {
				t.Fatal(database.SafeError("scan index", err))
			}
			if _, ok := actual[name]; ok {
				t.Fatalf("duplicate index name %s", name)
			}
			actual[name] = spec
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("index rows", err))
		}
		expected := expectedAuthIndexes()
		if len(actual) != len(expected) {
			t.Fatalf("index count=%d want=%d", len(actual), len(expected))
		}
		for name, want := range expected {
			got, ok := actual[name]
			if !ok {
				t.Fatalf("missing index %s", name)
			}
			if got.unique != want.unique || got.columns != want.columns {
				t.Fatalf("index %s unique=%t columns=%q want unique=%t columns=%q", name, got.unique, got.columns, want.unique, want.columns)
			}
		}
		t.Logf("auth indexes=%d all match", len(actual))
	})

	t.Run("forbidden objects", func(t *testing.T) {
		checks := []struct {
			name  string
			query string
		}{
			{"foreign key and check constraints", "SELECT count(*)::int FROM pg_constraint con JOIN pg_namespace n ON n.oid=con.connamespace WHERE n.nspname=current_schema() AND con.contype IN ('f','c')"},
			{"triggers", "SELECT count(*)::int FROM pg_trigger tg JOIN pg_class c ON c.oid=tg.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND NOT tg.tgisinternal"},
			{"row level security", "SELECT count(*)::int FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('users','sessions') AND c.relrowsecurity"},
		}
		for _, check := range checks {
			var count int
			if err := db.QueryRowContext(ctx, check.query).Scan(&count); err != nil {
				t.Fatal(database.SafeError(check.name, err))
			}
			if count != 0 {
				t.Fatalf("%s count=%d want=0", check.name, count)
			}
		}
	})

	t.Run("grants", func(t *testing.T) {
		query := "SELECT table_name::text, string_agg(privilege_type::text, ',' ORDER BY privilege_type)::text FROM information_schema.role_table_grants WHERE grantee='tokenpivot_app' AND table_schema=current_schema() AND table_name IN ('users','sessions') GROUP BY table_name"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("grants", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close grant rows failed")
			}
		}()
		actual := make(map[string]string)
		for rows.Next() {
			var table, privileges string
			if err := rows.Scan(&table, &privileges); err != nil {
				t.Fatal(database.SafeError("scan grant", err))
			}
			actual[table] = privileges
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("grant rows", err))
		}
		if len(actual) != len(authTables) {
			t.Fatalf("grant tables=%d want=%d", len(actual), len(authTables))
		}
		for _, table := range authTables {
			if actual[table] != "INSERT,SELECT,UPDATE" {
				t.Fatalf("table %s grants=%q want=%q", table, actual[table], "INSERT,SELECT,UPDATE")
			}
		}
	})

	t.Run("defaults and uniqueness", func(t *testing.T) {
		var userID string
		var createdAt time.Time
		var version int
		err := app.QueryRow(ctx, "INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id, created_at, uuid_extract_version(id)", "synthetic-auth@example.com", "synthetic-encoding").Scan(&userID, &createdAt, &version)
		if err != nil {
			t.Fatal(database.SafeError("insert user", err))
		}
		if version != 7 || createdAt.IsZero() {
			t.Fatalf("user defaults: uuid version=%d created_at zero=%t", version, createdAt.IsZero())
		}
		// Email equality is bytewise under the C collation, so case differs.
		if _, err := app.Exec(ctx, "INSERT INTO users (email, password_hash) VALUES ($1, $2)", "Synthetic-Auth@example.com", "synthetic-encoding"); err != nil {
			t.Fatal("the C collation treated differently cased emails as equal")
		}
		_, err = app.Exec(ctx, "INSERT INTO users (email, password_hash) VALUES ($1, $2)", "synthetic-auth@example.com", "synthetic-encoding")
		requireSQLState(t, err, "23505")

		digest := bytes.Repeat([]byte{0x5a}, 32)
		var sessionID string
		if err := app.QueryRow(ctx, "INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days') RETURNING id", userID, digest).Scan(&sessionID); err != nil {
			t.Fatal(database.SafeError("insert session", err))
		}
		_, err = app.Exec(ctx, "INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1, $2, now() + interval '30 days')", userID, digest)
		requireSQLState(t, err, "23505")
		// expires_at has no default, so a writer cannot omit it silently.
		_, err = app.Exec(ctx, "INSERT INTO sessions (user_id, token_hash) VALUES ($1, $2)", userID, bytes.Repeat([]byte{0x5b}, 32))
		requireSQLState(t, err, "23502")
	})

	t.Run("permission denial", func(t *testing.T) {
		for _, table := range authTables {
			for _, statement := range []string{
				"DELETE FROM " + table,
				"TRUNCATE " + table,
				"ALTER TABLE " + table + " ADD COLUMN forbidden_probe integer",
			} {
				_, err := app.Exec(ctx, statement)
				requireSQLState(t, err, "42501")
			}
		}
	})

	t.Run("rollback", func(t *testing.T) {
		if _, err := p.DownTo(ctx, usageMinuteStatsVersion); err != nil {
			t.Fatal(database.SafeError("auth down", err))
		}
		for _, table := range authTables {
			var absent bool
			if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.'||$1) IS NULL", table).Scan(&absent); err != nil {
				t.Fatal(database.SafeError("check dropped table", err))
			}
			if !absent {
				t.Fatalf("table %s survived rollback", table)
			}
		}
		var usageMinuteStats bool
		if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.usage_minute_stats') IS NOT NULL").Scan(&usageMinuteStats); err != nil || !usageMinuteStats {
			t.Fatal("down of 00005 removed an earlier migration's table")
		}
		version, err := p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(database.SafeError("version", err))
		}
		if version != usageMinuteStatsVersion {
			t.Fatalf("version=%d want=%d", version, usageMinuteStatsVersion)
		}
		if _, err := p.UpTo(ctx, authTablesVersion); err != nil {
			t.Fatal(database.SafeError("auth reapply", err))
		}
		version, err = p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(database.SafeError("version", err))
		}
		if version != authTablesVersion {
			t.Fatalf("version=%d want=%d", version, authTablesVersion)
		}
	})
}
