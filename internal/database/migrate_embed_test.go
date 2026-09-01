package database

import (
	"io/fs"
	"testing"

	"github.com/srikarguntaka/photo-organizer/migrations"
)

// realMigrations lives in its own file so the import of the migrations package
// (which would otherwise be an import cycle risk if database ever imported it
// non-test) stays clearly test-only.
func realMigrations(t *testing.T) fs.FS {
	t.Helper()
	return migrations.FS()
}
