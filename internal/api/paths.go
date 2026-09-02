package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// requireDirectory verifies a resolved path exists and is a directory, with
// error messages aimed at the person who typed the path.
//
// Returned to the client, so it must not leak anything beyond what the caller
// already supplied -- these run only on paths that already passed the
// containment check, so the path is one the caller named.
func requireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no such directory: %s", path)
		}
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied reading: %s", path)
		}
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a file, not a directory", path)
	}
	return nil
}

// defaultLibraryName derives a human name from a path when the caller did not
// supply one. Falls back to the full path for a root-level directory, since
// "photos" is a more useful label than "".
func defaultLibraryName(path string) string {
	base := filepath.Base(filepath.Clean(path))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return path
	}
	// Strip a Windows drive-letter colon so the name reads cleanly.
	return strings.TrimSuffix(base, ":")
}
