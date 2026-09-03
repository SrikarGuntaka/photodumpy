// Package photos holds the domain types for a photo library and the local
// filesystem discovery logic.
//
// It has no database import. Path handling, format classification and the
// directory walk are all pure enough to unit test with a temp dir and no
// Postgres, which is what keeps the security-critical containment checks cheap
// to test exhaustively.
package photos

import "time"

// SourceKind is where a library's photos come from.
//
// This exists from day one so the future iOS client is a new value rather than
// a schema migration. Everything downstream of discovery works on rows and
// readers, not on filesystem paths.
type SourceKind string

const (
	SourceLocalFS     SourceKind = "local_fs"
	SourceIOSPhotoKit SourceKind = "ios_photokit" // not implemented
)

// State is the coarse lifecycle of a photo. It is deliberately not a mirror of
// job status -- job progress lives in the jobs table. This answers "is this
// photo usable in the UI yet, and if not, why".
type State string

const (
	StateDiscovered State = "discovered"
	StateProcessing State = "processing"
	StateReady      State = "ready"
	StateMissing    State = "missing"
	StateFailed     State = "failed"
)

// Library is one user-chosen source of photos.
type Library struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	SourceKind SourceKind `json:"source_kind"`
	// RootPath is absolute on the machine running the API, already validated
	// to sit inside PHOTO_ROOT. Meaningful only for local_fs libraries.
	RootPath string `json:"root_path"`

	LastScanStartedAt  *time.Time `json:"last_scan_started_at,omitempty"`
	LastScanFinishedAt *time.Time `json:"last_scan_finished_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ScanState summarises where a library's scan stands, derived rather than
// stored so it cannot drift from the timestamps it describes.
func (l *Library) ScanState() string {
	switch {
	case l.LastScanStartedAt == nil:
		return "never_scanned"
	case l.LastScanFinishedAt == nil:
		return "scanning"
	case l.LastScanFinishedAt.Before(*l.LastScanStartedAt):
		// Started after it last finished: a scan is in flight.
		return "scanning"
	default:
		return "complete"
	}
}

// Photo is one discovered image file.
//
// Phase 2 populates only what a directory walk can know. Metadata, hashes and
// quality metrics arrive in later phases along with the columns that hold them.
type Photo struct {
	ID        string `json:"id"`
	LibraryID string `json:"library_id"`

	RelativePath     string `json:"relative_path"`
	OriginalFilename string `json:"original_filename"`

	FileSizeBytes  int64      `json:"file_size_bytes"`
	FileModifiedAt *time.Time `json:"file_modified_at,omitempty"`
	DetectedFormat *string    `json:"detected_format,omitempty"`

	// --- derived by metadata extraction (Phase 3) ------------------------
	// All nullable: a photo that has not been processed yet, or one whose
	// file carries no EXIF, legitimately has none of these.

	// Width and Height are display dimensions, already adjusted for
	// Orientation.
	Width       *int `json:"width,omitempty"`
	Height      *int `json:"height,omitempty"`
	Orientation *int `json:"orientation,omitempty"`

	CapturedAt *time.Time `json:"captured_at,omitempty"`
	// CapturedAtSource is "exif" or "filesystem". Consumers must check it
	// before treating CapturedAt as authoritative.
	CapturedAtSource *string `json:"captured_at_source,omitempty"`

	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`

	CameraMake  *string `json:"camera_make,omitempty"`
	CameraModel *string `json:"camera_model,omitempty"`

	// MetadataExtractedAt is the idempotency marker: non-null means
	// extraction has run, successfully or not.
	MetadataExtractedAt *time.Time `json:"metadata_extracted_at,omitempty"`

	State     State   `json:"state"`
	LastError *string `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ScanResult is what a completed scan reports back.
type ScanResult struct {
	LibraryID string `json:"library_id"`

	// Discovered is how many supported image files the walk found.
	Discovered int `json:"discovered"`
	// Inserted is how many were new. Re-scanning an unchanged library gives
	// Discovered > 0 and Inserted == 0, which is the idempotency guarantee
	// made visible rather than merely claimed.
	Inserted int `json:"inserted"`
	// AlreadyKnown is Discovered minus Inserted.
	AlreadyKnown int `json:"already_known"`

	SkippedUnsupported int `json:"skipped_unsupported"`
	SkippedHidden      int `json:"skipped_hidden"`
	SkippedTooLarge    int `json:"skipped_too_large"`
	Unreadable         int `json:"unreadable"`

	DurationMS int64 `json:"duration_ms"`
	// Interrupted is true when the scan stopped early (cancelled, or the API
	// shut down). The rows it did insert are still valid -- inserts are
	// idempotent, so re-running resumes rather than duplicating.
	Interrupted bool `json:"interrupted"`
}
