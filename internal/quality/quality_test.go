package quality

import (
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/srikarguntaka/photo-organizer/internal/fixtures"
)

func analyzeFile(t *testing.T, path string) Metrics {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return Analyze(img, DefaultThresholds())
}

func corpus(t *testing.T) (string, *fixtures.Manifest) {
	t.Helper()
	root := t.TempDir()
	m, err := fixtures.Generate(fixtures.Options{Root: root, Seed: 1, Scenes: 24})
	if err != nil {
		t.Fatalf("generating corpus: %v", err)
	}
	return root, m
}

// THE Phase 7 assertion: photos the corpus deliberately blurred must measure
// as less sharp than the ones it did not, with a clear margin.
func TestBlurredPhotosScoreLowerOnSharpness(t *testing.T) {
	root, m := corpus(t)

	var blurred, sharp []float64
	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CorruptStage != "" {
			continue
		}
		mt := analyzeFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if f.Blurry {
			blurred = append(blurred, mt.Sharpness)
			t.Logf("blurred  %-40s sharpness %.3f  (raw laplacian %.1f)",
				filepath.Base(f.RelPath), mt.Sharpness, mt.RawLaplacianVariance)
		} else if !f.Underexposed && !f.Overexposed {
			// Exposure extremes are excluded: crushing an image to near-black
			// also destroys the detail sharpness measures, so they are not a
			// fair comparison for a blur test.
			sharp = append(sharp, mt.Sharpness)
		}
	}

	if len(blurred) == 0 {
		t.Fatal("corpus contains no deliberately blurred photos")
	}
	if len(sharp) == 0 {
		t.Fatal("corpus contains no sharp photos to compare against")
	}

	sort.Float64s(blurred)
	sort.Float64s(sharp)
	worstSharp := sharp[0]
	bestBlurred := blurred[len(blurred)-1]

	t.Logf("blurred max %.3f, sharp min %.3f, separation %.3f",
		bestBlurred, worstSharp, worstSharp-bestBlurred)

	if bestBlurred >= worstSharp {
		t.Errorf("the sharpest blurred photo (%.3f) scores at or above the least sharp "+
			"in-focus photo (%.3f); the metric does not separate them",
			bestBlurred, worstSharp)
	}
}

// Every deliberately blurred photo must actually raise the flag, and no
// in-focus one may.
func TestBlurFlagMatchesGroundTruth(t *testing.T) {
	root, m := corpus(t)
	th := DefaultThresholds()

	var falseNegatives, falsePositives []string
	checked := 0

	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CorruptStage != "" {
			continue
		}
		// Exposure extremes legitimately lose detail; excluded, as above.
		if f.Underexposed || f.Overexposed {
			continue
		}

		mt := analyzeFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)))
		flagged := hasFlag(mt.Flags, FlagPossiblyBlurry)
		checked++

		if f.Blurry && !flagged {
			falseNegatives = append(falseNegatives,
				filepath.Base(f.RelPath)+" sharpness="+ftoa(mt.Sharpness))
		}
		if !f.Blurry && flagged {
			falsePositives = append(falsePositives,
				filepath.Base(f.RelPath)+" sharpness="+ftoa(mt.Sharpness))
		}
	}

	if checked == 0 {
		t.Fatal("no photos checked")
	}
	if len(falseNegatives) > 0 {
		t.Errorf("blurred photos NOT flagged at threshold %.2f: %v", th.Sharpness, falseNegatives)
	}
	if len(falsePositives) > 0 {
		t.Errorf("in-focus photos wrongly flagged blurry at threshold %.2f: %v",
			th.Sharpness, falsePositives)
	}
	t.Logf("checked %d photos, %d false negatives, %d false positives",
		checked, len(falseNegatives), len(falsePositives))
}

// Under- and over-exposed photos must be detected, and correctly-exposed ones
// left alone.
func TestExposureFlagsMatchGroundTruth(t *testing.T) {
	root, m := corpus(t)

	checked := 0
	for _, f := range m.Files {
		if f.Kind != "jpeg" || f.CorruptStage != "" {
			continue
		}
		mt := analyzeFile(t, filepath.Join(root, filepath.FromSlash(f.RelPath)))
		name := filepath.Base(f.RelPath)

		switch {
		case f.Underexposed:
			if !hasFlag(mt.Flags, FlagPossiblyUnderexposed) {
				t.Errorf("%s is deliberately dark but was not flagged (mean luminance %.1f)",
					name, mt.MeanLuminance)
			}
			if hasFlag(mt.Flags, FlagPossiblyOverexposed) {
				t.Errorf("%s is dark but was flagged overexposed", name)
			}
			checked++

		case f.Overexposed:
			if !hasFlag(mt.Flags, FlagPossiblyOverexposed) {
				t.Errorf("%s is deliberately bright but was not flagged (mean luminance %.1f)",
					name, mt.MeanLuminance)
			}
			if hasFlag(mt.Flags, FlagPossiblyUnderexposed) {
				t.Errorf("%s is bright but was flagged underexposed", name)
			}
			checked++

		default:
			if hasFlag(mt.Flags, FlagPossiblyUnderexposed) || hasFlag(mt.Flags, FlagPossiblyOverexposed) {
				t.Errorf("%s is normally exposed but was flagged %v (mean luminance %.1f)",
					name, mt.Flags, mt.MeanLuminance)
			}
		}
	}
	if checked == 0 {
		t.Fatal("corpus contains no exposure extremes")
	}
	t.Logf("verified %d exposure extremes", checked)
}

// Scores must be comparable across resolutions, or a threshold means nothing
// in a library shot on several devices. This is why analysis happens at a
// fixed size rather than natively.
func TestSharpnessIsComparableAcrossResolutions(t *testing.T) {
	full := detailedImage(1600, 1200)
	half := scaleTo(full, 800, 600)
	quarter := scaleTo(full, 400, 300)

	mf := Analyze(full, DefaultThresholds())
	mh := Analyze(half, DefaultThresholds())
	mq := Analyze(quarter, DefaultThresholds())

	t.Logf("1600x1200 sharpness %.3f, 800x600 %.3f, 400x300 %.3f",
		mf.Sharpness, mh.Sharpness, mq.Sharpness)

	// The same image at different sizes should land in the same ballpark.
	// Not identical -- downscaling genuinely removes some detail -- but close
	// enough that a threshold does not flip.
	if diff := abs(mf.Sharpness - mh.Sharpness); diff > 0.30 {
		t.Errorf("halving resolution moved sharpness by %.3f (%.3f -> %.3f); scores must be "+
			"comparable across a library shot at different sizes", diff, mf.Sharpness, mh.Sharpness)
	}
	if mf.Sharpness < 0.15 || mh.Sharpness < 0.15 {
		t.Errorf("a detailed image scored as blurry: %.3f and %.3f", mf.Sharpness, mh.Sharpness)
	}
	_ = mq
}

// A blurred copy of an image must score below the original, which is the
// property the near-duplicate ranking will rely on.
func TestBlurMeasurablyReducesSharpness(t *testing.T) {
	sharp := detailedImage(800, 600)
	blurred := boxBlur(sharp, 5)

	ms := Analyze(sharp, DefaultThresholds())
	mb := Analyze(blurred, DefaultThresholds())

	t.Logf("sharp raw laplacian %.1f -> blurred %.1f  (%.1fx reduction)",
		ms.RawLaplacianVariance, mb.RawLaplacianVariance,
		ms.RawLaplacianVariance/max(mb.RawLaplacianVariance, 0.001))

	if mb.Sharpness >= ms.Sharpness {
		t.Errorf("blurring did not reduce sharpness: %.3f -> %.3f", ms.Sharpness, mb.Sharpness)
	}
	if mb.RawLaplacianVariance >= ms.RawLaplacianVariance {
		t.Error("blurring did not reduce raw Laplacian variance")
	}
}

// A flat image has no detail to measure and must score zero rather than
// dividing by something and producing a NaN.
func TestFlatImageScoresZeroSharpness(t *testing.T) {
	flat := solidImage(200, 200, 128)
	m := Analyze(flat, DefaultThresholds())

	if m.Sharpness != 0 {
		t.Errorf("a solid colour scored sharpness %.4f, want 0", m.Sharpness)
	}
	if m.Contrast != 0 {
		t.Errorf("a solid colour scored contrast %.4f, want 0", m.Contrast)
	}
	if !hasFlag(m.Flags, FlagPossiblyBlurry) {
		t.Error("a solid colour was not flagged as possibly blurry")
	}
	if isNaN(m.Overall) {
		t.Error("Overall is NaN for a flat image")
	}
}

// Pure black and pure white must be detected as clipped, and must not produce
// NaN or out-of-range scores.
func TestClippingDetection(t *testing.T) {
	black := solidImage(200, 200, 0)
	white := solidImage(200, 200, 255)

	mb := Analyze(black, DefaultThresholds())
	if mb.ShadowClipping < 0.99 {
		t.Errorf("pure black measured %.2f shadow clipping, want ~1.0", mb.ShadowClipping)
	}
	if !hasFlag(mb.Flags, FlagShadowsClipped) {
		t.Error("pure black did not raise the shadow clipping flag")
	}
	if !hasFlag(mb.Flags, FlagPossiblyUnderexposed) {
		t.Error("pure black was not flagged underexposed")
	}

	mw := Analyze(white, DefaultThresholds())
	if mw.HighlightClipping < 0.99 {
		t.Errorf("pure white measured %.2f highlight clipping, want ~1.0", mw.HighlightClipping)
	}
	if !hasFlag(mw.Flags, FlagHighlightsClipped) {
		t.Error("pure white did not raise the highlight clipping flag")
	}
	if !hasFlag(mw.Flags, FlagPossiblyOverexposed) {
		t.Error("pure white was not flagged overexposed")
	}
}

// Every score must stay inside its documented range, whatever the input.
func TestScoresStayInRange(t *testing.T) {
	inputs := []image.Image{
		solidImage(100, 100, 0),
		solidImage(100, 100, 255),
		solidImage(100, 100, 128),
		detailedImage(400, 300),
		boxBlur(detailedImage(400, 300), 8),
		solidImage(1, 1, 100),   // degenerate: too small for a Laplacian
		solidImage(2, 2, 100),   // degenerate
		detailedImage(3000, 40), // extreme aspect ratio
	}

	for i, img := range inputs {
		m := Analyze(img, DefaultThresholds())

		for name, v := range map[string]float64{
			"Sharpness":  m.Sharpness,
			"Exposure":   m.Exposure,
			"Contrast":   m.Contrast,
			"Resolution": m.Resolution,
			"Overall":    m.Overall,
		} {
			if isNaN(v) {
				t.Errorf("input %d: %s is NaN", i, name)
			}
			if v < 0 || v > 1 {
				t.Errorf("input %d: %s = %.4f, outside 0..1", i, name, v)
			}
		}
		if m.MeanLuminance < 0 || m.MeanLuminance > 255 {
			t.Errorf("input %d: MeanLuminance = %.2f, outside 0..255", i, m.MeanLuminance)
		}
	}
}

// The overall score is a weighted mean, not a product, so one weak dimension
// does not zero out a photo that is fine in every other respect.
func TestOverallIsAMeanNotAProduct(t *testing.T) {
	// Sharp and well exposed, but genuinely low contrast -- a foggy landscape.
	lowContrast := Metrics{
		Sharpness: 0.9, Exposure: 0.9, Contrast: 0.02, Resolution: 1.0,
	}
	got := overallScore(lowContrast)

	if got < 0.5 {
		t.Errorf("a sharp, well-exposed, low-contrast photo scored %.3f; a product would "+
			"crush it toward zero, but low contrast is often the subject rather than a defect", got)
	}

	// Sharpness carries the most weight, being the least recoverable.
	blurry := overallScore(Metrics{Sharpness: 0.05, Exposure: 0.9, Contrast: 0.9, Resolution: 1.0})
	dark := overallScore(Metrics{Sharpness: 0.9, Exposure: 0.05, Contrast: 0.9, Resolution: 1.0})
	if blurry >= dark {
		t.Errorf("a blurry photo (%.3f) scored at or above an underexposed one (%.3f); "+
			"focus cannot be recovered in an editor, exposure often can", blurry, dark)
	}
}

// Analysis must be deterministic: the same bytes must produce the same scores,
// or at-least-once job delivery would write different values on a re-run.
func TestAnalysisIsDeterministic(t *testing.T) {
	img := detailedImage(600, 400)
	first := Analyze(img, DefaultThresholds())

	for i := 0; i < 5; i++ {
		got := Analyze(img, DefaultThresholds())
		if got.Sharpness != first.Sharpness ||
			got.Exposure != first.Exposure ||
			got.Contrast != first.Contrast ||
			got.Overall != first.Overall {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
}

// Thresholds are configuration, so changing them must change the flags.
func TestThresholdsAreConfigurable(t *testing.T) {
	img := boxBlur(detailedImage(800, 600), 4)

	strict := DefaultThresholds()
	strict.Sharpness = 0.99
	if !hasFlag(Analyze(img, strict).Flags, FlagPossiblyBlurry) {
		t.Error("a near-maximum sharpness threshold did not flag a blurred image")
	}

	lenient := DefaultThresholds()
	lenient.Sharpness = 0.0
	if hasFlag(Analyze(img, lenient).Flags, FlagPossiblyBlurry) {
		t.Error("a zero sharpness threshold still flagged an image as blurry")
	}
}

// Flags must be an empty slice rather than nil, so the API renders [].
func TestFlagsAreNeverNil(t *testing.T) {
	m := Analyze(detailedImage(1600, 1200), DefaultThresholds())
	if m.Flags == nil {
		t.Error("Flags is nil; it must be an empty slice so JSON renders [] not null")
	}
}

func BenchmarkAnalyze(b *testing.B) {
	img := detailedImage(4000, 3000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Analyze(img, DefaultThresholds())
	}
}

// --- helpers --------------------------------------------------------------

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

func ftoa(f float64) string { return fmt.Sprintf("%.3f", f) }

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func isNaN(f float64) bool { return f != f }

// Regression test for a real ranking failure.
//
// Sharpness must DECREASE as an image is downscaled, because a thumbnail
// genuinely contains less detail than its source. The original implementation
// left images below analysisSize at native resolution, measuring them on a
// denser grid: a 160x120 thumbnail scored 2686 against 71.5 for its
// 1600x1200 source, and the near-duplicate ranking recommended keeping a
// thumbnail over its own original.
func TestDownscalingReducesSharpness(t *testing.T) {
	source := detailedImage(1600, 1200)

	sizes := [][2]int{{1600, 1200}, {800, 600}, {640, 480}, {320, 240}, {160, 120}}
	var prev float64 = -1
	var prevName string

	for _, d := range sizes {
		m := Analyze(scaleTo(source, d[0], d[1]), DefaultThresholds())
		name := fmt.Sprintf("%dx%d", d[0], d[1])
		t.Logf("%-10s sharpness %.3f  (raw laplacian %.1f)", name, m.Sharpness, m.RawLaplacianVariance)

		if prev >= 0 && m.Sharpness > prev+0.05 {
			t.Errorf("%s scored %.3f, ABOVE the larger %s at %.3f -- downscaling must not "+
				"increase measured sharpness, or a thumbnail outranks its own source",
				name, m.Sharpness, prevName, prev)
		}
		prev, prevName = m.Sharpness, name
	}
}
