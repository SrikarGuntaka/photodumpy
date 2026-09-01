// Package migrations embeds the SQL migration files so the binaries are
// self-contained. There is no separate migration tool to install and no risk of
// the running binary and the SQL on disk drifting apart.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS returns the embedded migrations rooted at the directory containing the
// .sql files.
func FS() fs.FS { return files }
