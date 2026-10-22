package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// openRawDB opens a database configured like Open does, without migrating it.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "raw.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func pragmaInt(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func testMigrations(sqls ...string) []migration {
	ms := make([]migration, len(sqls))
	for i, s := range sqls {
		ms[i] = migration{version: i + 1, name: "test", sql: s}
	}
	return ms
}

func TestEmbeddedMigrationsAreValid(t *testing.T) {
	ms, err := loadMigrations(migrationFiles, migrationDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
	db := openRawDB(t)
	if err := migrate(t.Context(), db, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := pragmaInt(t, db, "user_version"); got != len(ms) {
		t.Errorf("user_version = %d, want %d", got, len(ms))
	}
}

func TestLoadMigrations(t *testing.T) {
	file := &fstest.MapFile{Data: []byte("SELECT 1;")}
	tests := []struct {
		name     string
		files    []string
		wantErr  string
		wantVers int
	}{
		{name: "ordered", files: []string{"0001_a.sql", "0002_b.sql", "0003_c.sql"}, wantVers: 3},
		{name: "empty", files: nil, wantVers: 0},
		{name: "gap", files: []string{"0001_a.sql", "0003_c.sql"}, wantErr: "version 3, want 2"},
		{name: "does not start at 1", files: []string{"0002_b.sql"}, wantErr: "version 2, want 1"},
		{name: "duplicate version", files: []string{"0001_a.sql", "0001_b.sql"}, wantErr: "version 1, want 2"},
		{name: "bad name", files: []string{"1_a.sql"}, wantErr: "NNNN_description.sql"},
		{name: "wrong extension", files: []string{"0001_a.txt"}, wantErr: "NNNN_description.sql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The directory entry keeps "m" present when there are no files.
			fsys := fstest.MapFS{"m": &fstest.MapFile{Mode: fs.ModeDir}}
			for _, f := range tt.files {
				fsys["m/"+f] = file
			}
			ms, err := loadMigrations(fsys, "m")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(ms) != tt.wantVers {
				t.Errorf("loaded %d migrations, want %d", len(ms), tt.wantVers)
			}
		})
	}
}

func TestMigrateIsIncremental(t *testing.T) {
	db := openRawDB(t)
	first := `CREATE TABLE a (id INTEGER PRIMARY KEY);`
	// Without IF NOT EXISTS, running a migration twice fails.
	second := `CREATE TABLE b (id INTEGER PRIMARY KEY);`

	if err := migrate(t.Context(), db, testMigrations(first)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := migrate(t.Context(), db, testMigrations(first, second)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if err := migrate(t.Context(), db, testMigrations(first, second)); err != nil {
		t.Fatalf("up-to-date run: %v", err)
	}
	if got := pragmaInt(t, db, "user_version"); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
	if got := pragmaInt(t, db, "foreign_keys"); got != 1 {
		t.Errorf("foreign_keys = %d after migrating, want 1", got)
	}
}

func TestMigrateRollsBackFailures(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		wantErr string
	}{
		{
			name:    "invalid statement",
			sql:     `CREATE TABLE b (id INTEGER PRIMARY KEY); THIS IS NOT SQL;`,
			wantErr: "syntax error",
		},
		{
			name: "foreign key violation",
			sql: `CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER REFERENCES a(id));
			      INSERT INTO b (a_id) VALUES (42);`,
			wantErr: "foreign key violation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openRawDB(t)
			ms := testMigrations(`CREATE TABLE a (id INTEGER PRIMARY KEY);`, tt.sql)
			err := migrate(t.Context(), db, ms)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if got := pragmaInt(t, db, "user_version"); got != 1 {
				t.Errorf("user_version = %d, want 1", got)
			}
			if !tableExists(t, db, "a") || tableExists(t, db, "b") {
				t.Error("failed migration was not rolled back")
			}
			if got := pragmaInt(t, db, "foreign_keys"); got != 1 {
				t.Errorf("foreign_keys = %d after a failure, want 1", got)
			}
		})
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	db := openRawDB(t)
	if _, err := db.ExecContext(t.Context(), `PRAGMA user_version = 5`); err != nil {
		t.Fatal(err)
	}
	err := migrate(t.Context(), db, testMigrations(`SELECT 1;`))
	if err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Errorf("error = %v, want a newer-schema error", err)
	}
}
