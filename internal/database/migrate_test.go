package database

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadOrdersByVersionNotFilename(t *testing.T) {
	// Lexical ordering of these names is 0002, 0010, 0001 -- if load sorted by
	// filename instead of parsed version, migration 10 would run before 1.
	fsys := fstest.MapFS{
		"0010_third.sql":  {Data: []byte("SELECT 3;")},
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
	}

	got, err := load(fsys)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	want := []int{1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("load() returned %d migrations, want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i].Version != v {
			t.Errorf("migration[%d].Version = %d, want %d", i, got[i].Version, v)
		}
	}
}

func TestLoadParsesNameAndChecksum(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_core_schema.sql": {Data: []byte("CREATE TABLE t ();")},
	}

	got, err := load(fsys)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if got[0].Name != "core_schema" {
		t.Errorf("Name = %q, want core_schema", got[0].Name)
	}
	if len(got[0].Checksum) != 64 {
		t.Errorf("Checksum = %q, want a 64-char hex sha256", got[0].Checksum)
	}

	// The checksum must depend on content -- that is the entire point of the
	// drift detection in Migrate.
	fsys2 := fstest.MapFS{"0001_core_schema.sql": {Data: []byte("CREATE TABLE u ();")}}
	got2, err := load(fsys2)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if got[0].Checksum == got2[0].Checksum {
		t.Error("different migration bodies produced the same checksum")
	}
}

// A file that does not parse must be an error. Skipping it silently would mean
// a migration that never runs and a schema that is quietly wrong.
func TestLoadRejectsBadFilenames(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"no version prefix":  {"core.sql": {Data: []byte("SELECT 1;")}},
		"non-numeric prefix": {"abc_core.sql": {Data: []byte("SELECT 1;")}},
		"leading underscore": {"_core.sql": {Data: []byte("SELECT 1;")}},
		"duplicate version": {
			"0001_a.sql": {Data: []byte("SELECT 1;")},
			"0001_b.sql": {Data: []byte("SELECT 2;")},
		},
	}
	for name, fsys := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := load(fsys); err == nil {
				t.Error("load() succeeded; want an error")
			}
		})
	}
}

// Non-SQL files sitting in the migrations directory (embed.go itself, a README)
// must be ignored rather than treated as malformed migrations.
func TestLoadIgnoresNonSQLFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_core.sql": {Data: []byte("SELECT 1;")},
		"embed.go":      {Data: []byte("package migrations")},
		"README.md":     {Data: []byte("# notes")},
	}
	got, err := load(fsys)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("load() returned %d migrations, want 1", len(got))
	}
}

// The real embedded migrations must parse. This catches a mis-named file the
// moment it is added, without needing a database.
func TestEmbeddedMigrationsAreValid(t *testing.T) {
	got, err := load(realMigrations(t))
	if err != nil {
		t.Fatalf("embedded migrations failed to load: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no embedded migrations found")
	}
	for i, m := range got {
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %04d_%s is empty", m.Version, m.Name)
		}
		if i > 0 && got[i-1].Version >= m.Version {
			t.Errorf("migrations are not strictly increasing at index %d", i)
		}
	}
}
