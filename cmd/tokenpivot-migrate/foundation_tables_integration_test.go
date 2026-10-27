//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	bootstrapVersion  = 1
	foundationVersion = 2
)

var foundationTables = []string{"workspaces", "projects", "sources", "ingest_credentials"}

// foundationColumn is the expected shape of one information_schema.columns row.
// defaultText is a substring of column_default; the empty string means NULL.
// collation is the expected collation_name; the empty string means NULL.
type foundationColumn struct {
	dataType    string
	nullable    string
	defaultText string
	collation   string
}

func expectedFoundationColumns() map[string]foundationColumn {
	id := foundationColumn{dataType: "uuid", nullable: "NO", defaultText: "uuidv7()"}
	createdAt := foundationColumn{dataType: "timestamp with time zone", nullable: "NO", defaultText: "now()"}
	workspaceID := foundationColumn{dataType: "uuid", nullable: "NO"}
	return map[string]foundationColumn{
		"workspaces.id":                   id,
		"workspaces.name":                 {dataType: "text", nullable: "NO"},
		"workspaces.created_at":           createdAt,
		"projects.id":                     id,
		"projects.workspace_id":           workspaceID,
		"projects.project_key":            {dataType: "text", nullable: "NO", collation: "C"},
		"projects.name":                   {dataType: "text", nullable: "NO"},
		"projects.archived_at":            {dataType: "timestamp with time zone", nullable: "YES"},
		"projects.created_at":             createdAt,
		"sources.id":                      id,
		"sources.workspace_id":            workspaceID,
		"sources.platform":                {dataType: "text", nullable: "NO", collation: "C"},
		"sources.instance_key":            {dataType: "text", nullable: "NO", collation: "C"},
		"sources.last_received_at":        {dataType: "timestamp with time zone", nullable: "YES"},
		"sources.created_at":              createdAt,
		"ingest_credentials.id":           id,
		"ingest_credentials.workspace_id": workspaceID,
		"ingest_credentials.source_id":    {dataType: "uuid", nullable: "NO"},
		"ingest_credentials.secret_hash":  {dataType: "bytea", nullable: "NO"},
		"ingest_credentials.revoked_at":   {dataType: "timestamp with time zone", nullable: "YES"},
		"ingest_credentials.created_at":   createdAt,
	}
}

// foundationIndex is the expected shape of one pg_index row.
type foundationIndex struct {
	unique  bool
	columns string
}

func expectedFoundationIndexes() map[string]foundationIndex {
	return map[string]foundationIndex{
		"workspaces_pkey":                               {unique: true, columns: "id"},
		"projects_pkey":                                 {unique: true, columns: "id"},
		"uk_projects_workspace_id_project_key":          {unique: true, columns: "workspace_id,project_key"},
		"sources_pkey":                                  {unique: true, columns: "id"},
		"uk_sources_workspace_id_platform_instance_key": {unique: true, columns: "workspace_id,platform,instance_key"},
		"ingest_credentials_pkey":                       {unique: true, columns: "id"},
		"uk_ingest_credentials_secret_hash":             {unique: true, columns: "secret_hash"},
		"idx_ingest_credentials_workspace_id_source_id": {unique: false, columns: "workspace_id,source_id"},
	}
}

// TestFoundationTables pins itself to migration 00002 with UpTo/DownTo so later
// migrations cannot change these assertions.
func TestFoundationTables(t *testing.T) {
	migrationURL, appURL := integrationSchema(t)
	p, db := testProvider(t, migrationURL, migrations.Files)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := p.UpTo(ctx, foundationVersion); err != nil {
		t.Fatal(database.SafeError("foundation up", err))
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
		query := "SELECT table_name::text, column_name::text, data_type::text, is_nullable::text, coalesce(column_default,'')::text, coalesce(collation_name,'')::text FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('workspaces','projects','sources','ingest_credentials')"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("columns", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close column rows failed")
			}
		}()
		actual := make(map[string]foundationColumn)
		for rows.Next() {
			var table, column string
			var spec foundationColumn
			if err := rows.Scan(&table, &column, &spec.dataType, &spec.nullable, &spec.defaultText, &spec.collation); err != nil {
				t.Fatal(database.SafeError("scan column", err))
			}
			actual[table+"."+column] = spec
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("column rows", err))
		}
		expected := expectedFoundationColumns()
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
		t.Logf("columns=%d all match", len(actual))
	})

	t.Run("indexes", func(t *testing.T) {
		query := "SELECT c.relname::text, i.relname::text, ix.indisunique, (SELECT string_agg(a.attname, ',' ORDER BY u.k) FROM unnest(string_to_array(ix.indkey::text, ' ')::int[]) WITH ORDINALITY AS u(attnum, k) JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=u.attnum)::text FROM pg_index ix JOIN pg_class i ON i.oid=ix.indexrelid JOIN pg_class c ON c.oid=ix.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('workspaces','projects','sources','ingest_credentials')"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("indexes", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close index rows failed")
			}
		}()
		actual := make(map[string]foundationIndex)
		for rows.Next() {
			var table string
			var name string
			var spec foundationIndex
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
		expected := expectedFoundationIndexes()
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
		t.Logf("indexes=%d all match", len(actual))
	})

	t.Run("forbidden objects", func(t *testing.T) {
		checks := []struct {
			name  string
			query string
		}{
			{"foreign key and check constraints", "SELECT count(*)::int FROM pg_constraint con JOIN pg_namespace n ON n.oid=con.connamespace WHERE n.nspname=current_schema() AND con.contype IN ('f','c')"},
			{"triggers", "SELECT count(*)::int FROM pg_trigger tg JOIN pg_class c ON c.oid=tg.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND NOT tg.tgisinternal"},
			{"row level security", "SELECT count(*)::int FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname IN ('workspaces','projects','sources','ingest_credentials') AND c.relrowsecurity"},
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
		query := "SELECT table_name::text, string_agg(privilege_type::text, ',' ORDER BY privilege_type)::text FROM information_schema.role_table_grants WHERE grantee='tokenpivot_app' AND table_schema=current_schema() GROUP BY table_name"
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
		if len(actual) != len(foundationTables) {
			t.Fatalf("grant tables=%d want=%d", len(actual), len(foundationTables))
		}
		for _, table := range foundationTables {
			if actual[table] != "INSERT,SELECT,UPDATE" {
				t.Fatalf("table %s grants=%q want=%q", table, actual[table], "INSERT,SELECT,UPDATE")
			}
		}
		t.Logf("grants=INSERT,SELECT,UPDATE on %d tables", len(actual))
	})

	t.Run("defaults", func(t *testing.T) {
		var id string
		var createdAt time.Time
		var version int
		err := app.QueryRow(ctx, "INSERT INTO workspaces (name) VALUES ($1) RETURNING id, created_at, uuid_extract_version(id)", "synthetic-defaults").Scan(&id, &createdAt, &version)
		if err != nil {
			t.Fatal(database.SafeError("insert workspace", err))
		}
		if version != 7 {
			t.Fatalf("uuid version=%d want=7", version)
		}
		if createdAt.IsZero() {
			t.Fatal("created_at was not defaulted")
		}
		t.Logf("workspace id version=%d created_at defaulted", version)
	})

	t.Run("project uniqueness", func(t *testing.T) {
		ws1 := insertWorkspace(t, ctx, app, "synthetic-projects-a")
		ws2 := insertWorkspace(t, ctx, app, "synthetic-projects-b")
		insertProject(t, ctx, app, ws1, "k")
		var err error
		_, err = app.Exec(ctx, "INSERT INTO projects (workspace_id, project_key, name) VALUES ($1, $2, $3)", ws1, "k", "synthetic-duplicate")
		requireSQLState(t, err, "23505")
		insertProject(t, ctx, app, ws2, "k")
		if _, err := app.Exec(ctx, "UPDATE projects SET archived_at=now() WHERE workspace_id=$1 AND project_key=$2", ws1, "k"); err != nil {
			t.Fatal(database.SafeError("archive project", err))
		}
		_, err = app.Exec(ctx, "INSERT INTO projects (workspace_id, project_key, name) VALUES ($1, $2, $3)", ws1, "k", "synthetic-archived")
		requireSQLState(t, err, "23505")
		insertProject(t, ctx, app, ws1, "K")
	})

	t.Run("source uniqueness", func(t *testing.T) {
		ws1 := insertWorkspace(t, ctx, app, "synthetic-sources-a")
		ws2 := insertWorkspace(t, ctx, app, "synthetic-sources-b")
		insertSource(t, ctx, app, ws1, "synthetic-platform", "instance-1")
		var err error
		_, err = app.Exec(ctx, "INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1, $2, $3)", ws1, "synthetic-platform", "instance-1")
		requireSQLState(t, err, "23505")
		insertSource(t, ctx, app, ws1, "synthetic-other-platform", "instance-1")
		insertSource(t, ctx, app, ws2, "synthetic-platform", "instance-1")
	})

	t.Run("rotation and revocation", func(t *testing.T) {
		ws := insertWorkspace(t, ctx, app, "synthetic-rotation")
		sourceID := insertSource(t, ctx, app, ws, "synthetic-platform", "instance-rotate")
		hashA := bytes.Repeat([]byte{0xa1}, 32)
		hashB := bytes.Repeat([]byte{0xb2}, 32)
		first := insertCredential(t, ctx, app, ws, sourceID, hashA)
		insertCredential(t, ctx, app, ws, sourceID, hashB)
		_, err := app.Exec(ctx, "INSERT INTO ingest_credentials (workspace_id, source_id, secret_hash) VALUES ($1, $2, $3)", ws, sourceID, hashA)
		requireSQLState(t, err, "23505")
		tag, err := app.Exec(ctx, "UPDATE ingest_credentials SET revoked_at=now() WHERE id=$1", first)
		if err != nil {
			t.Fatal(database.SafeError("revoke credential", err))
		}
		if affected := tag.RowsAffected(); affected != 1 {
			t.Fatalf("revoked rows=%d want=1", affected)
		}
		var id, workspaceID, platform, instanceKey string
		if err := app.QueryRow(ctx, "SELECT id, workspace_id, platform, instance_key FROM sources WHERE id=$1", sourceID).Scan(&id, &workspaceID, &platform, &instanceKey); err != nil {
			t.Fatal(database.SafeError("reload source", err))
		}
		if id != sourceID || workspaceID != ws || platform != "synthetic-platform" || instanceKey != "instance-rotate" {
			t.Fatalf("source identity changed: id=%s workspace=%s platform=%q instance=%q", id, workspaceID, platform, instanceKey)
		}
		tag, err = app.Exec(ctx, "UPDATE sources SET last_received_at=now() WHERE id=$1", sourceID)
		if err != nil {
			t.Fatal(database.SafeError("touch source", err))
		}
		if affected := tag.RowsAffected(); affected != 1 {
			t.Fatalf("touched rows=%d want=1", affected)
		}
	})

	t.Run("permission denial", func(t *testing.T) {
		for _, table := range foundationTables {
			for _, statement := range []string{
				"DELETE FROM " + table,
				"TRUNCATE " + table,
				fmt.Sprintf("ALTER TABLE %s ADD COLUMN forbidden_probe integer", table),
			} {
				_, err := app.Exec(ctx, statement)
				requireSQLState(t, err, "42501")
			}
		}
		t.Logf("DELETE, TRUNCATE and ALTER TABLE denied on %d tables", len(foundationTables))
	})

	t.Run("rollback", func(t *testing.T) {
		if _, err := p.DownTo(ctx, bootstrapVersion); err != nil {
			t.Fatal(database.SafeError("foundation down", err))
		}
		for _, table := range foundationTables {
			var absent bool
			if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.'||$1) IS NULL", table).Scan(&absent); err != nil {
				t.Fatal(database.SafeError("check dropped table", err))
			}
			if !absent {
				t.Fatalf("table %s survived rollback", table)
			}
		}
		version, err := p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(database.SafeError("version", err))
		}
		if version != bootstrapVersion {
			t.Fatalf("version=%d want=%d", version, bootstrapVersion)
		}
		var history bool
		if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.goose_db_version') IS NOT NULL").Scan(&history); err != nil || !history {
			t.Fatal("goose history table missing after rollback")
		}
		if _, err := p.UpTo(ctx, foundationVersion); err != nil {
			t.Fatal(database.SafeError("foundation reapply", err))
		}
		version, err = p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(database.SafeError("version", err))
		}
		if version != foundationVersion {
			t.Fatalf("version=%d want=%d", version, foundationVersion)
		}
	})
}

func insertWorkspace(t *testing.T, ctx context.Context, app *pgx.Conn, name string) string {
	t.Helper()
	var id string
	if err := app.QueryRow(ctx, "INSERT INTO workspaces (name) VALUES ($1) RETURNING id", name).Scan(&id); err != nil {
		t.Fatal(database.SafeError("insert workspace", err))
	}
	return id
}

func insertProject(t *testing.T, ctx context.Context, app *pgx.Conn, workspaceID, key string) {
	t.Helper()
	if _, err := app.Exec(ctx, "INSERT INTO projects (workspace_id, project_key, name) VALUES ($1, $2, $3)", workspaceID, key, "synthetic-project"); err != nil {
		t.Fatal(database.SafeError("insert project", err))
	}
}

func insertSource(t *testing.T, ctx context.Context, app *pgx.Conn, workspaceID, platform, instanceKey string) string {
	t.Helper()
	var id string
	if err := app.QueryRow(ctx, "INSERT INTO sources (workspace_id, platform, instance_key) VALUES ($1, $2, $3) RETURNING id", workspaceID, platform, instanceKey).Scan(&id); err != nil {
		t.Fatal(database.SafeError("insert source", err))
	}
	return id
}

func insertCredential(t *testing.T, ctx context.Context, app *pgx.Conn, workspaceID, sourceID string, secretHash []byte) string {
	t.Helper()
	var id string
	if err := app.QueryRow(ctx, "INSERT INTO ingest_credentials (workspace_id, source_id, secret_hash) VALUES ($1, $2, $3) RETURNING id", workspaceID, sourceID, secretHash).Scan(&id); err != nil {
		t.Fatal(database.SafeError("insert credential", err))
	}
	return id
}

func requireSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Fatalf("expected SQLSTATE %s, got %v", want, err)
	}
}
