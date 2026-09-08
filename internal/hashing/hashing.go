// Package hashing computes content hashes for photo files.
//
// Pure: takes a reader, returns bytes. No database, no filesystem walking, so
// it is testable with an in-memory reader and verifiable against known vectors.
package hashing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// Size is the length of a SHA-256 digest in bytes.
const Size = sha256.Size

// bufferSize is the chunk size used when streaming a file through the hash.
//
// 64KB is comfortably larger than a disk read-ahead window and small enough
// that a dozen concurrent hashers use under a megabyte between them. The point
// is that it is CONSTANT: memory does not scale with file size, so a 200MB
// panorama costs the same as a 2MB snapshot.
const bufferSize = 64 * 1024

// Sum computes the SHA-256 of everything r yields.
//
// The file is streamed, never buffered whole. That is the difference between
// hashing a 10,000-photo library in bounded memory and trying to hold a
// multi-gigabyte working set. io.CopyBuffer with an explicit buffer avoids
// io.Copy's per-call allocation, which matters when this runs once per photo
// across a large library.
//
// SHA-256 rather than a faster non-cryptographic hash (xxhash, CRC): two files
// with the same digest are treated as identical and one is suggested for
// deletion. A collision would mean recommending the user delete a photo that is
// not actually a duplicate. Cryptographic collision resistance is what makes
// "same hash therefore same file" a claim worth acting on, and hashing is
// I/O-bound here anyway -- the disk read dominates, so the faster hash would
// not measurably help.
func Sum(r io.Reader) ([]byte, error) {
	h := sha256.New()
	buf := make([]byte, bufferSize)

	if _, err := io.CopyBuffer(h, r, buf); err != nil {
		return nil, fmt.Errorf("hashing: reading input: %w", err)
	}
	return h.Sum(nil), nil
}

// SumWithSize computes the digest and also reports how many bytes were read.
//
// The byte count comes free from the copy, and having it lets a caller verify
// the file is the size the database expects -- a mismatch means the file
// changed since it was scanned.
func SumWithSize(r io.Reader) ([]byte, int64, error) {
	h := sha256.New()
	buf := make([]byte, bufferSize)

	n, err := io.CopyBuffer(h, r, buf)
	if err != nil {
		return nil, 0, fmt.Errorf("hashing: reading input: %w", err)
	}
	return h.Sum(nil), n, nil
}

// Hex renders a digest for display and logging. The database stores raw bytes;
// humans read hex.
func Hex(sum []byte) string { return hex.EncodeToString(sum) }

// Short renders the first 12 hex characters, enough to identify a group in a
// CLI listing without wrapping the line.
func Short(sum []byte) string {
	s := Hex(sum)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
