package quality

import (
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

// TestCalibrateQuality measures the corpus so thresholds are chosen from data.
//
//	go test -run TestCalibrateQuality -v ./internal/quality/
func TestCalibrateQuality(t *testing.T) {
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatal(err)
	}

	type sample struct {
		name   string
		raw    float64
		mean   float64
		hiClip float64
		loClip float64
		stdDev float64
	}
	var blurred, sharp, dark, bright []sample

	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CorruptStage != "" {
			continue
		}
		fh, err := os.Open(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		img, err := jpeg.Decode(fh)
		fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		mt := Analyze(img, DefaultThresholds())
		s := sample{filepath.Base(f.RelPath), mt.RawLaplacianVariance, mt.MeanLuminance,
			mt.HighlightClipping, mt.ShadowClipping, mt.RawStdDev}

		switch {
		case f.Blurry:
			blurred = append(blurred, s)
		case f.Underexposed:
			dark = append(dark, s)
		case f.Overexposed:
			bright = append(bright, s)
		default:
			sharp = append(sharp, s)
		}
	}

	report := func(label string, ss []sample) {
		if len(ss) == 0 {
			return
		}
		raws := make([]float64, len(ss))
		means := make([]float64, len(ss))
		for i, s := range ss {
			raws[i] = s.raw
			means[i] = s.mean
		}
		sort.Float64s(raws)
		sort.Float64s(means)
		t.Logf("%-9s n=%2d  laplacian min %6.1f max %6.1f  |  mean-lum min %5.1f max %5.1f",
			label, len(ss), raws[0], raws[len(raws)-1], means[0], means[len(means)-1])
	}

	report("sharp", sharp)
	report("blurred", blurred)
	report("dark", dark)
	report("bright", bright)

	t.Log("")
	for _, s := range bright {
		t.Logf("  bright  %-26s mean %.1f  highlight-clip %.3f  stddev %.1f",
			s.name, s.mean, s.hiClip, s.stdDev)
	}
	for _, s := range dark {
		t.Logf("  dark    %-26s mean %.1f  shadow-clip    %.3f  stddev %.1f",
			s.name, s.mean, s.loClip, s.stdDev)
	}

	// Reference: the synthetic detailed image used in unit tests.
	ref := Analyze(detailedImage(1600, 1200), DefaultThresholds())
	refBlur := Analyze(boxBlur(detailedImage(1600, 1200), 6), DefaultThresholds())
	t.Logf("")
	t.Logf("synthetic detailed image: laplacian %.1f, blurred %.1f",
		ref.RawLaplacianVariance, refBlur.RawLaplacianVariance)
}
