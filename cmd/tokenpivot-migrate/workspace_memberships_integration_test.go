//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
)

const (
	membershipsVersion       = 6
	authTablesVersionForT019 = 5
)

var membershipsTables = []string{"workspace_memberships"}

// membershipColumn is the expected shape of one information_schema.columns row.
type membershipColumn struct {
	dataType    string
	nullable    string
	defaultText string
	collation   string
}

// expectedMembershipColumns pins the four released columns exactly: no id, no
// type, no status, no soft-delete column and no created_by.
func expectedMembershipColumns() map[string]membershipColumn {
	return map[string]membershipColumn{
		"workspace_memberships.workspace_id": {dataType: "uuid", nullable: "NO"},
		"workspace_memberships.user_id":      {dataType: "uuid", nullable: "NO"},
		"workspace_memberships.role":         {dataType: "text", nullable: "NO"},
		"workspace_memberships.joined_at":    {dataType: "timestamp with time zone", nullable: "NO", defaultText: "now()"},
	}
}

// membershipIndex is the expected shape of one pg_index row.
type membershipIndex struct {
	unique  bool
	columns string
}

// expectedMembershipIndexes pins both indexes and their column order. There is
// deliberately no unique index on user_id alone: a user may belong to several
// workspaces.
func expectedMembershipIndexes() map[string]membershipIndex {
	return map[string]membershipIndex{
		"uk_workspace_memberships_workspace_id_user_id": {unique: true, columns: "workspace_id,user_id"},
		"idx_workspace_memberships_user_id":             {unique: false, columns: "user_id"},
	}
}

// TestWorkspaceMemberships pins itself with UpTo/DownTo so later migrations
// cannot change these assertions.
func TestWorkspaceMemberships(t *testing.T) {
	migrationURL, appURL := integrationSchema(t)
	p, db := testProvider(t, migrationURL, migrations.Files)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := p.UpTo(ctx, membershipsVersion); err != nil {
		t.Fatal(database.SafeError("memberships up", err))
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
		query := "SELECT table_name::text, column_name::text, data_type::text, is_nullable::text, coalesce(column_default,'')::text, coalesce(collation_name,'')::text FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_memberships'"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("columns", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close column rows failed")
			}
		}()
		actual := make(map[string]membershipColumn)
		for rows.Next() {
			var table, column string
			var spec membershipColumn
			if err := rows.Scan(&table, &column, &spec.dataType, &spec.nullable, &spec.defaultText, &spec.collation); err != nil {
				t.Fatal(database.SafeError("scan column", err))
			}
			actual[table+"."+column] = spec
		}
		if err := rows.Err(); err != nil {
			t.Fatal(database.SafeError("column rows", err))
		}
		expected := expectedMembershipColumns()
		if len(actual) != len(expected) {
			t.Fatalf("column count=%d want=%d (%v)", len(actual), len(expected), actual)
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
		var missing int
		if err := db.QueryRowContext(ctx, "SELECT count(*)::int FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='workspace_memberships' AND is_nullable='YES'").Scan(&missing); err != nil {
			t.Fatal(database.SafeError("nullable columns", err))
		}
		if missing != 0 {
			t.Fatalf("nullable columns=%d want=0", missing)
		}
		t.Logf("membership columns=%d all match", len(actual))
	})

	t.Run("indexes", func(t *testing.T) {
		query := "SELECT c.relname::text, i.relname::text, ix.indisunique, (SELECT string_agg(a.attname, ',' ORDER BY u.k) FROM unnest(string_to_array(ix.indkey::text, ' ')::int[]) WITH ORDINALITY AS u(attnum, k) JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=u.attnum)::text FROM pg_index ix JOIN pg_class i ON i.oid=ix.indexrelid JOIN pg_class c ON c.oid=ix.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='workspace_memberships'"
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(database.SafeError("indexes", err))
		}
		defer func() {
			if err := rows.Close(); err != nil {
				t.Error("close index rows failed")
			}
		}()
		actual := make(map[string]membershipIndex)
		for rows.Next() {
			var table, name string
			var spec membershipIndex
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
		expected := expectedMembershipIndexes()
		if len(actual) != len(expected) {
			t.Fatalf("index count=%d want=%d (%v)", len(actual), len(expected), actual)
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
		// No index may be unique on user_id alone, which would forbid the future
		// multi-workspace membership the data model already allows.
		for name, spec := range actual {
			if spec.unique && spec.columns == "user_id" {
				t.Fatalf("index %s wrongly makes user_id unique", name)
			}
		}
		t.Logf("membership indexes=%d all match", len(actual))
	})

	t.Run("forbidden objects", func(t *testing.T) {
		checks := []struct {
			name  string
			query string
		}{
			{"foreign key and check constraints", "SELECT count(*)::int FROM pg_constraint con JOIN pg_namespace n ON n.oid=con.connamespace WHERE n.nspname=current_schema() AND con.contype IN ('f','c')"},
			{"triggers", "SELECT count(*)::int FROM pg_trigger tg JOIN pg_class c ON c.oid=tg.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND NOT tg.tgisinternal"},
			{"row level security", "SELECT count(*)::int FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='workspace_memberships' AND c.relrowsecurity"},
			{"enum types", "SELECT count(*)::int FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname=current_schema() AND t.typtype='e'"},
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

	t.Run("comments", func(t *testing.T) {
		query := "SELECT coalesce(obj_description(c.oid,'pg_class'),'')::text, coalesce(col_description(c.oid,(SELECT attnum FROM pg_attribute WHERE attrelid=c.oid AND attname='role')),'')::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='workspace_memberships'"
		var table, role string
		if err := db.QueryRowContext(ctx, query).Scan(&table, &role); err != nil {
			t.Fatal(database.SafeError("comments", err))
		}
		if !strings.Contains(table, "owner rows for personal workspaces") {
			t.Fatalf("table comment=%q does not state the personal-only write boundary", table)
		}
		if !strings.Contains(role, "owner or member") {
			t.Fatalf("role comment=%q does not list the accepted roles", role)
		}
	})

	t.Run("grants", func(t *testing.T) {
		query := "SELECT table_name::text, string_agg(privilege_type::text, ',' ORDER BY privilege_type)::text FROM information_schema.role_table_grants WHERE grantee='tokenpivot_app' AND table_schema=current_schema() AND table_name IN ('workspace_memberships','workspaces','users') GROUP BY table_name"
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
		if actual["workspace_memberships"] != "INSERT,SELECT" {
			t.Fatalf("workspace_memberships grants=%q want %q", actual["workspace_memberships"], "INSERT,SELECT")
		}
		// The later migrations keep their own grants; creation still needs both.
		if actual["workspaces"] != "INSERT,SELECT,UPDATE" {
			t.Fatalf("workspaces grants=%q", actual["workspaces"])
		}
		if actual["users"] != "INSERT,SELECT,UPDATE" {
			t.Fatalf("users grants=%q", actual["users"])
		}
	})

	// The seed helpers below run as the service role, so they also prove the
	// full chain is usable: users lock, workspace insert and membership insert.
	t.Run("defaults and uniqueness", func(t *testing.T) {
		first := seedUser(t, ctx, app, "membership-first@example.com")
		second := seedUser(t, ctx, app, "membership-second@example.com")
		workspace := seedWorkspace(t, ctx, app, "synthetic-membership")

		var joinedAt time.Time
		if err := app.QueryRow(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3) RETURNING joined_at", workspace, first, "owner").Scan(&joinedAt); err != nil {
			t.Fatal(database.SafeError("insert membership", err))
		}
		if joinedAt.IsZero() {
			t.Fatal("joined_at default was not applied")
		}
		_, err := app.Exec(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)", workspace, first, "member")
		requireSQLState(t, err, "23505")
		// A different user may join the same workspace.
		if _, err := app.Exec(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)", workspace, second, "member"); err != nil {
			t.Fatal("a second user could not join the same workspace")
		}
		// The same user may own another workspace: no user_id unique index.
		other := seedWorkspace(t, ctx, app, "synthetic-membership-other")
		if _, err := app.Exec(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)", other, first, "owner"); err != nil {
			t.Fatal("a user could not hold a membership in a second workspace")
		}
		// role has no CHECK: any text is accepted here and validated by the app.
		third := seedUser(t, ctx, app, "membership-third@example.com")
		if _, err := app.Exec(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)", other, third, "synthetic-role"); err != nil {
			t.Fatal("role column must stay plain text without a CHECK")
		}
		// No foreign keys: an unknown user id is accepted by the database, so
		// referential integrity is an application guarantee, not a schema one.
		if _, err := app.Exec(ctx, "INSERT INTO workspace_memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)", other, "00000000-0000-7000-8000-000000000000", "owner"); err != nil {
			t.Fatal("the database rejected an orphan membership, so a foreign key exists")
		}
	})

	// effective privileges answers what the service connection may actually do
	// after PUBLIC and inherited roles are folded in: information_schema only
	// lists direct grants, so it cannot prove the absence of a wider path.
	t.Run("effective privileges", func(t *testing.T) {
		privileges := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
		row := app.QueryRow(ctx, `SELECT
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'SELECT'),
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'INSERT'),
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'UPDATE'),
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'DELETE'),
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'TRUNCATE'),
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'REFERENCES'),
has_table_privilege(current_user, current_schema()||'.workspace_memberships', 'TRIGGER')`)
		actual := make([]bool, len(privileges))
		dest := make([]any, len(privileges))
		for i := range actual {
			dest[i] = &actual[i]
		}
		if err := row.Scan(dest...); err != nil {
			t.Fatal(database.SafeError("effective privileges", err))
		}
		for i, name := range privileges {
			want := name == "SELECT" || name == "INSERT"
			if actual[i] != want {
				t.Errorf("%s privilege on workspace_memberships=%t want %t", name, actual[i], want)
			}
		}
	})

	t.Run("permission denial", func(t *testing.T) {
		for _, statement := range []string{
			"DELETE FROM workspace_memberships",
			"TRUNCATE workspace_memberships",
			"UPDATE workspace_memberships SET role = 'member'",
			"ALTER TABLE workspace_memberships ADD COLUMN forbidden_probe integer",
		} {
			_, err := app.Exec(ctx, statement)
			requireSQLState(t, err, "42501")
		}
	})

	t.Run("rollback", func(t *testing.T) {
		if _, err := p.DownTo(ctx, authTablesVersionForT019); err != nil {
			t.Fatal(database.SafeError("memberships down", err))
		}
		for _, table := range membershipsTables {
			var absent bool
			if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.'||$1) IS NULL", table).Scan(&absent); err != nil {
				t.Fatal(database.SafeError("check dropped table", err))
			}
			if !absent {
				t.Fatalf("table %s survived rollback", table)
			}
		}
		// Down of 00006 must leave the earlier tables and their rows in place.
		var users, workspaces bool
		if err := db.QueryRowContext(ctx, "SELECT to_regclass(current_schema()||'.users') IS NOT NULL, to_regclass(current_schema()||'.workspaces') IS NOT NULL").Scan(&users, &workspaces); err != nil {
			t.Fatal(database.SafeError("earlier tables", err))
		}
		if !users || !workspaces {
			t.Fatal("down of 00006 removed an earlier migration's table")
		}
		version, err := p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(database.SafeError("version", err))
		}
		if version != authTablesVersionForT019 {
			t.Fatalf("version=%d want=%d", version, authTablesVersionForT019)
		}
		if _, err := p.UpTo(ctx, membershipsVersion); err != nil {
			t.Fatal(database.SafeError("memberships reapply", err))
		}
		version, err = p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(database.SafeError("version", err))
		}
		if version != membershipsVersion {
			t.Fatalf("version=%d want=%d", version, membershipsVersion)
		}
	})
}

// seedUser inserts one synthetic account as the service role.
func seedUser(t *testing.T, ctx context.Context, app *pgx.Conn, email string) string {
	t.Helper()
	var userID string
	if err := app.QueryRow(ctx, "INSERT INTO users (email, password_hash) VALUES ($1,$2) RETURNING id", email, "synthetic-encoding").Scan(&userID); err != nil {
		t.Fatal(database.SafeError("seed user", err))
	}
	return userID
}

// seedWorkspace inserts one synthetic workspace as the service role.
func seedWorkspace(t *testing.T, ctx context.Context, app *pgx.Conn, name string) string {
	t.Helper()
	var workspaceID string
	if err := app.QueryRow(ctx, "INSERT INTO workspaces (name) VALUES ($1) RETURNING id", name).Scan(&workspaceID); err != nil {
		t.Fatal(database.SafeError("seed workspace", err))
	}
	return workspaceID
}
