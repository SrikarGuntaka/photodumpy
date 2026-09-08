//go:build integration

// Integration tests for ingestion. These need a real Postgres, because the
// behaviour under test IS the database behaviour: ON CONFLICT idempotency, the
// claim-by-update scan guard, and batch insert counts. Mocking the store would
// test the mock.
//
// Run with:  go test -tags integration ./internal/ingest/
// TEST_DATABASE_URL must point at a database that may be freely modified.
package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/srikarguntaka/photo-organizer/internal/database"
	"github.com/srikarguntaka/photo-organizer/internal/photos"
	"github.com/srikarguntaka/photo-organizer/internal/store"
	"github.com/srikarguntaka/photo-organizer/migrations"
	"log/slog"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pool, err := database.Connect(ctx, dsn, 8, 30*time.Second, log)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	if err := database.Migrate(ctx, pool, migrations.FS(), log); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newLibrary creates an isolated library so parallel tests do not collide.
func newLibrary(t *testing.T, st *store.Store, root string) *photos.Library {
	t.Helper()
	name := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())

	lib, _, err := st.CreateLibrary(context.Background(), name, root, photos.SourceLocalFS)
	if err != nil {
		t.Fatalf("creating library: %v", err)
	}
	t.Cleanup(func() {
		// ON DELETE CASCADE removes the photos too.
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM libraries WHERE id = $1`, lib.ID)
	})
	return lib
}

func buildTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func newScanner(t *testing.T, st *store.Store) *Scanner {
	t.Helper()
	return NewScanner(st, testLogger(t))
}

// testLogger keeps integration test output readable -- warnings and above only.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestScanInsertsDiscoveredPhotos(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{
		"a.jpg":         "1",
		"sub/b.png":     "2",
		"sub/c.webp":    "3",
		"notes.txt":     "ignored",
		"sub/movie.mp4": "ignored",
	})
	lib := newLibrary(t, st, root)

	result, err := newScanner(t, st).ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatalf("ScanSync error = %v", err)
	}

	if result.Discovered != 3 {
		t.Errorf("Discovered = %d, want 3", result.Discovered)
	}
	if result.Inserted != 3 {
		t.Errorf("Inserted = %d, want 3", result.Inserted)
	}
	if result.SkippedUnsupported != 2 {
		t.Errorf("SkippedUnsupported = %d, want 2", result.SkippedUnsupported)
	}

	count, err := st.CountPhotos(context.Background(), lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("database holds %d photos, want 3", count)
	}
}

// THE idempotency guarantee. Re-scanning an unchanged folder must insert
// nothing, and the counts must say so rather than us asserting it.
func TestRescanIsIdempotent(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{
		"a.jpg": "1", "b.jpg": "2", "sub/c.png": "3",
	})
	lib := newLibrary(t, st, root)
	sc := newScanner(t, st)

	first, err := sc.ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	if first.Inserted != 3 {
		t.Fatalf("first scan Inserted = %d, want 3", first.Inserted)
	}

	second, err := sc.ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	if second.Discovered != 3 {
		t.Errorf("second scan Discovered = %d, want 3 (it still sees the files)", second.Discovered)
	}
	if second.Inserted != 0 {
		t.Errorf("second scan Inserted = %d, want 0 -- rescanning must not duplicate rows", second.Inserted)
	}
	if second.AlreadyKnown != 3 {
		t.Errorf("second scan AlreadyKnown = %d, want 3", second.AlreadyKnown)
	}

	count, _ := st.CountPhotos(context.Background(), lib.ID)
	if count != 3 {
		t.Errorf("database holds %d photos after two scans, want 3", count)
	}
}

// Adding files and rescanning must pick up only the new ones.
func TestRescanPicksUpNewFiles(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{"a.jpg": "1"})
	lib := newLibrary(t, st, root)
	sc := newScanner(t, st)

	if _, err := sc.ScanSync(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "b.jpg"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new", "c.png"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := sc.ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	if second.Inserted != 2 {
		t.Errorf("Inserted = %d, want 2 (only the new files)", second.Inserted)
	}
	if second.Discovered != 3 {
		t.Errorf("Discovered = %d, want 3", second.Discovered)
	}
}

// A file deleted after a scan leaves its row behind. Phase 5 marks such rows
// 'missing' when a worker fails to read them; the scan itself must not delete
// rows, because a temporarily unmounted drive would otherwise wipe the library.
func TestScanDoesNotRemoveRowsForDeletedFiles(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{"a.jpg": "1", "b.jpg": "2"})
	lib := newLibrary(t, st, root)
	sc := newScanner(t, st)

	if _, err := sc.ScanSync(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "b.jpg")); err != nil {
		t.Fatal(err)
	}

	second, err := sc.ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatal(err)
	}
	if second.Discovered != 1 {
		t.Errorf("Discovered = %d, want 1", second.Discovered)
	}

	count, _ := st.CountPhotos(context.Background(), lib.ID)
	if count != 2 {
		t.Errorf("database holds %d photos, want 2 -- a scan must not delete rows for "+
			"files that have disappeared (an unmounted drive would wipe the library)", count)
	}
}

// Batching must not change the result. Crossing the batch boundary is where an
// off-by-one in the flush logic would show up.
func TestScanHandlesMoreFilesThanOneBatch(t *testing.T) {
	st := store.New(testPool(t))

	files := map[string]string{}
	total := batchSize + 37 // deliberately not a multiple of batchSize
	for i := 0; i < total; i++ {
		files[fmt.Sprintf("d%02d/photo%04d.jpg", i%10, i)] = fmt.Sprintf("content-%d", i)
	}
	root := buildTree(t, files)
	lib := newLibrary(t, st, root)

	result, err := newScanner(t, st).ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatalf("ScanSync error = %v", err)
	}
	if result.Discovered != total {
		t.Errorf("Discovered = %d, want %d", result.Discovered, total)
	}
	if result.Inserted != total {
		t.Errorf("Inserted = %d, want %d -- a file was lost at a batch boundary", result.Inserted, total)
	}

	count, _ := st.CountPhotos(context.Background(), lib.ID)
	if count != total {
		t.Errorf("database holds %d photos, want %d", count, total)
	}
}

// A cancelled scan keeps whatever it wrote and reports itself interrupted;
// resuming completes the job without duplicating anything.
func TestCancelledScanIsResumable(t *testing.T) {
	st := store.New(testPool(t))

	files := map[string]string{}
	total := batchSize * 3
	for i := 0; i < total; i++ {
		files[fmt.Sprintf("d%02d/p%04d.jpg", i%10, i)] = fmt.Sprintf("c%d", i)
	}
	root := buildTree(t, files)
	lib := newLibrary(t, st, root)
	sc := newScanner(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel partway through so at least one batch has been flushed.
	go func() {
		for {
			n, _ := st.CountPhotos(context.Background(), lib.ID)
			if n >= batchSize {
				cancel()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	first, err := sc.ScanSync(ctx, lib)
	if err != nil {
		t.Fatalf("a cancelled scan should not error: %v", err)
	}
	if !first.Interrupted {
		t.Log("scan completed before cancellation landed; the resume half of this test still applies")
	}

	// Resume with a fresh context.
	second, err := sc.ScanSync(context.Background(), lib)
	if err != nil {
		t.Fatalf("resumed scan error = %v", err)
	}
	if second.Discovered != total {
		t.Errorf("resumed scan Discovered = %d, want %d", second.Discovered, total)
	}

	count, _ := st.CountPhotos(context.Background(), lib.ID)
	if count != total {
		t.Errorf("database holds %d photos after cancel+resume, want %d", count, total)
	}
}

// The scan guard lives in the UPDATE's WHERE clause, so exactly one of two
// concurrent starts can win.
func TestOnlyOneScanCanStartAtATime(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{"a.jpg": "1"})
	lib := newLibrary(t, st, root)

	first, err := st.MarkScanStarted(context.Background(), lib.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first MarkScanStarted did not claim the scan")
	}

	second, err := st.MarkScanStarted(context.Background(), lib.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("second MarkScanStarted also claimed the scan; the guard is not working")
	}

	// force overrides, which is how a scan stuck by a crash is recovered.
	forced, err := st.MarkScanStarted(context.Background(), lib.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !forced {
		t.Error("force=true failed to claim a stuck scan")
	}

	// After finishing, a normal start works again.
	if err := st.MarkScanFinished(context.Background(), lib.ID); err != nil {
		t.Fatal(err)
	}
	again, err := st.MarkScanStarted(context.Background(), lib.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !again {
		t.Error("could not start a scan after the previous one finished")
	}
}

// A library left mid-scan by a crashed process must be recoverable, or it can
// never be scanned again.
func TestReconcileInterruptedScans(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{"a.jpg": "1"})
	lib := newLibrary(t, st, root)

	if _, err := st.MarkScanStarted(context.Background(), lib.ID, false); err != nil {
		t.Fatal(err)
	}

	before, err := st.GetLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ScanState() != "scanning" {
		t.Fatalf("ScanState = %q, want scanning", before.ScanState())
	}

	n, err := st.ReconcileInterruptedScans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("reconciled %d libraries, want at least 1", n)
	}

	after, err := st.GetLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ScanState() != "complete" {
		t.Errorf("ScanState = %q after reconciliation, want complete", after.ScanState())
	}
}

// Creating a library for the same path twice returns the same row.
func TestCreateLibraryIsIdempotent(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{"a.jpg": "1"})

	first, created1, err := st.CreateLibrary(context.Background(), "first", root, photos.SourceLocalFS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM libraries WHERE id = $1`, first.ID)
	})
	if !created1 {
		t.Error("first CreateLibrary reported created=false")
	}

	second, created2, err := st.CreateLibrary(context.Background(), "second", root, photos.SourceLocalFS)
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Error("second CreateLibrary reported created=true; it must return the existing row")
	}
	if second.ID != first.ID {
		t.Errorf("second call returned id %s, want the existing %s", second.ID, first.ID)
	}
	if second.Name != "first" {
		t.Errorf("Name = %q, want the original %q -- a rescan must not rename the library",
			second.Name, "first")
	}
}

// Photo rows must carry what the walk learned, in the normalised form.
func TestScannedPhotoFieldsArePersisted(t *testing.T) {
	st := store.New(testPool(t))
	root := buildTree(t, map[string]string{"sub/deep/photo.jpg": "hello world"})
	lib := newLibrary(t, st, root)

	if _, err := newScanner(t, st).ScanSync(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListPhotos(context.Background(), lib.ID, store.ListPhotosOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d photos, want 1", len(list))
	}
	p := list[0]

	if p.RelativePath != "sub/deep/photo.jpg" {
		t.Errorf("RelativePath = %q, want sub/deep/photo.jpg (forward slashes)", p.RelativePath)
	}
	if p.OriginalFilename != "photo.jpg" {
		t.Errorf("OriginalFilename = %q, want photo.jpg", p.OriginalFilename)
	}
	if p.FileSizeBytes != int64(len("hello world")) {
		t.Errorf("FileSizeBytes = %d, want %d", p.FileSizeBytes, len("hello world"))
	}
	if p.State != photos.StateDiscovered {
		t.Errorf("State = %q, want discovered", p.State)
	}
	if p.DetectedFormat == nil || *p.DetectedFormat != "jpeg" {
		t.Errorf("DetectedFormat = %v, want jpeg", p.DetectedFormat)
	}
	if p.FileModifiedAt == nil {
		t.Error("FileModifiedAt is nil; the scan should record filesystem mtime as the " +
			"fallback capture time for photos with no EXIF")
	}
}
