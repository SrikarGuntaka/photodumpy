package hashing

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

// Known vectors. If these ever fail, the implementation is not SHA-256 and
// every stored hash in every database is wrong.
func TestKnownVectors(t *testing.T) {
	tests := map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"The quick brown fox jumps over the lazy dog": "d7a8fbb307d7809469ca9abcb0082e4f8d5651e46d3cdb762d02d0bf37c9e592",
	}
	for input, want := range tests {
		got, err := Sum(strings.NewReader(input))
		if err != nil {
			t.Errorf("Sum(%q) error = %v", input, err)
			continue
		}
		if Hex(got) != want {
			t.Errorf("Sum(%q) = %s, want %s", input, Hex(got), want)
		}
	}
}

// The streaming implementation must agree with the one-shot library function
// regardless of size, especially across the internal buffer boundary where a
// chunking bug would hide.
func TestStreamingMatchesOneShot(t *testing.T) {
	sizes := []int{
		0, 1, 1023,
		bufferSize - 1, bufferSize, bufferSize + 1,
		bufferSize * 3,
		bufferSize*3 + 7, // deliberately not a multiple
	}

	for _, size := range sizes {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i * 31 % 251)
		}

		got, err := Sum(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("size %d: Sum error = %v", size, err)
		}
		want := sha256.Sum256(data)

		if !bytes.Equal(got, want[:]) {
			t.Errorf("size %d: streaming digest differs from sha256.Sum256", size)
		}
	}
}

func TestSumWithSizeReportsBytesRead(t *testing.T) {
	data := bytes.Repeat([]byte("x"), bufferSize*2+123)

	sum, n, err := SumWithSize(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("SumWithSize error = %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("read %d bytes, want %d", n, len(data))
	}
	want := sha256.Sum256(data)
	if !bytes.Equal(sum, want[:]) {
		t.Error("digest differs from sha256.Sum256")
	}
}

func TestDigestIsCorrectLength(t *testing.T) {
	// The schema has CHECK (length(sha256) = 32), so a change to either the
	// constant or the digest length must fail here rather than at INSERT time.
	if Size != 32 {
		t.Fatalf("Size = %d, want 32 -- the migration's CHECK constraint hardcodes 32", Size)
	}

	sum, err := Sum(strings.NewReader("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != Size {
		t.Errorf("digest length = %d, want %d", len(sum), Size)
	}
}

// A read failure must surface, not produce a hash of the partial content. A
// truncated read that silently returned a digest would mark two different files
// as duplicates.
func TestReadErrorPropagates(t *testing.T) {
	r := io.MultiReader(
		strings.NewReader("some data"),
		&failingReader{},
	)
	if _, err := Sum(r); err == nil {
		t.Error("Sum succeeded despite a read error; a partial hash must never be returned")
	}
}

type failingReader struct{}

func (f *failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// The real test: against the fixture corpus, whose duplicate groups are known
// by construction. Files the manifest says are byte-identical must hash
// identically, and files in different groups must not collide.
func TestCorpusDuplicateGroupsHashConsistently(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatalf("generating corpus: %v", err)
	}

	hashOf := func(rel string) string {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("opening %s: %v", rel, err)
		}
		defer f.Close()
		sum, err := Sum(f)
		if err != nil {
			t.Fatalf("hashing %s: %v", rel, err)
		}
		return Hex(sum)
	}

	groups := map[string][]string{}
	for _, f := range m.Files {
		if f.DuplicateGroup != "" {
			groups[f.DuplicateGroup] = append(groups[f.DuplicateGroup], f.RelPath)
		}
	}
	if len(groups) == 0 {
		t.Fatal("corpus contains no duplicate groups")
	}

	seen := map[string]string{}
	totalFiles := 0

	for name, paths := range groups {
		if len(paths) < 2 {
			t.Errorf("group %s has %d member(s), want >= 2", name, len(paths))
			continue
		}
		totalFiles += len(paths)

		first := hashOf(paths[0])
		for _, p := range paths[1:] {
			if h := hashOf(p); h != first {
				t.Errorf("group %s: %s hashes %s but %s hashes %s -- the manifest says "+
					"these are byte-identical", name, paths[0], first[:12], p, h[:12])
			}
		}

		if prev, dup := seen[first]; dup {
			t.Errorf("groups %s and %s share digest %s; they should be distinct", prev, name, first[:12])
		}
		seen[first] = name
	}

	if len(groups) != 3 {
		t.Errorf("found %d duplicate groups, corpus is built with 3", len(groups))
	}
	if totalFiles != 9 {
		t.Errorf("found %d duplicate files, corpus is built with 9", totalFiles)
	}
	t.Logf("verified %d groups covering %d byte-identical files", len(groups), totalFiles)
}

// Near-duplicates must NOT collide. This is the boundary between Phase 4 and
// Phase 6: SHA-256 is supposed to miss a recompressed copy, which is precisely
// why perceptual hashing exists.
func TestNearDuplicatesDoNotShareAHash(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatal(err)
	}

	groups := map[string][]string{}
	for _, f := range m.Files {
		if f.SimilarGroup != "" {
			groups[f.SimilarGroup] = append(groups[f.SimilarGroup], f.RelPath)
		}
	}
	if len(groups) == 0 {
		t.Fatal("corpus contains no near-duplicate groups")
	}

	for name, paths := range groups {
		digests := map[string]string{}
		for _, p := range paths {
			f, err := os.Open(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				t.Fatal(err)
			}
			sum, err := Sum(f)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			h := Hex(sum)
			if prev, clash := digests[h]; clash {
				t.Errorf("group %s: %s and %s share a SHA-256. They are visually similar "+
					"but must differ byte-wise, or the perceptual-hash path is never exercised",
					name, prev, p)
			}
			digests[h] = p
		}
	}
}

func TestShortAndHex(t *testing.T) {
	sum, err := Sum(strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Hex(sum); len(got) != 64 {
		t.Errorf("Hex length = %d, want 64", len(got))
	}
	if got := Short(sum); got != "ba7816bf8f01" {
		t.Errorf("Short = %q, want ba7816bf8f01", got)
	}
	if got := Short([]byte{0xab}); got != "ab" {
		t.Errorf("Short of a stub digest = %q, want ab (must not panic on short input)", got)
	}
}

func BenchmarkSum1MB(b *testing.B) {
	data := bytes.Repeat([]byte("photo bytes "), 1<<20/12)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := Sum(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
