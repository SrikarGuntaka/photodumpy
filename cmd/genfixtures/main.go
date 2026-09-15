// Command genfixtures writes a synthetic photo corpus with known ground truth.
//
//	go run ./cmd/genfixtures -root ./sample-photos -scenes 24
//
// The corpus is regenerated deterministically from -seed, so the same flags
// always produce byte-identical files.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

func main() {
	root := flag.String("root", "./sample-photos", "directory to write the corpus into")
	seed := flag.Int64("seed", 1, "random seed; same seed produces identical output")
	scenes := flag.Int("scenes", 24, "number of distinct base images")
	width := flag.Int("width", 640, "base image width in pixels")
	height := flag.Int("height", 480, "base image height in pixels")
	clean := flag.Bool("clean", false, "delete the target directory first")
	flag.Parse()

	if *clean {
		if err := os.RemoveAll(*root); err != nil {
			fmt.Fprintf(os.Stderr, "error: removing %s: %v\n", *root, err)
			os.Exit(1)
		}
	}

	m, err := fixtures.Generate(fixtures.Options{
		Root: *root, Seed: *seed, Scenes: *scenes, Width: *width, Height: *height,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	s := m.Summary
	fmt.Printf("Wrote %d files to %s\n\n", s.TotalFiles, *root)
	fmt.Printf("  supported images        %d\n", s.SupportedImages)
	fmt.Printf("  unsupported (skipped)   %d\n", s.UnsupportedFiles)
	fmt.Printf("  corrupt / undecodable   %d\n", s.CorruptFiles)
	fmt.Println()
	fmt.Printf("  exact duplicate groups  %d  (%d files)\n", s.ExactDuplicateGroups, s.ExactDuplicateFiles)
	fmt.Printf("  near-duplicate groups   %d\n", s.NearDuplicateGroups)
	fmt.Println()
	fmt.Printf("  with GPS                %d\n", s.WithGPS)
	fmt.Printf("  without GPS             %d\n", s.WithoutGPS)
	fmt.Printf("  without EXIF timestamp  %d\n", s.WithoutEXIF)
	fmt.Println()
	fmt.Printf("  intentionally blurry    %d\n", s.IntentionallyBlurry)
	fmt.Printf("  underexposed            %d\n", s.Underexposed)
	fmt.Printf("  overexposed             %d\n", s.Overexposed)
	fmt.Printf("\nGround truth written to %s/MANIFEST.json\n", *root)
}
