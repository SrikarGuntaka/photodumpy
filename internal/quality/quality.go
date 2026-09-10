// Package quality measures objective, technical properties of an image:
// how much fine detail it contains, how its tones are distributed, and how
// much of it is clipped to pure black or white.
//
// WHAT THIS PACKAGE DOES NOT DO, and will not:
//
// It does not decide whether a photo is good. Blur is measurable; beauty is
// not. A shallow depth-of-field portrait scores as "blurry" on Laplacian
// variance because most of the frame genuinely is blurred -- and it may be the
// best photograph in the library. Motion blur is sometimes the entire point. A
// deliberately high-key image clips highlights on purpose.
//
// So every output here is a statement about pixels, and every flag is hedged
// in its own name (PossiblyBlurry, not Blurry). The raw scores are always
// exposed alongside the flags so a user can disagree with the threshold rather
// than being told a verdict.
//
// Pure: takes an image, returns numbers. No database, no filesystem.
package quality

import (
	"image"
	"math"

	"golang.org/x/image/draw"
)

// analysisSize is the long-edge length every image is scaled to before
// measurement.
//
// THIS IS NOT ONLY A SPEED OPTIMISATION. Laplacian variance is resolution
// dependent: the same photograph at 12 megapixels and at 2 megapixels produces
// different variances, because downscaling averages away exactly the
// high-frequency detail the operator measures. Scoring photos at their native
// sizes would mean a library's sharpness scores said more about camera
// resolution than about focus.
//
// Normalising to a fixed analysis size makes scores broadly comparable across a
// library shot on several devices, which is the only way a threshold can mean
// anything. 512 keeps enough detail to distinguish focus while costing a
// fraction of a full-resolution pass.
//
// EVERY image is scaled to this size, including ones already smaller. That
// detail is load-bearing and was wrong at first.
//
// The original rule left small images at native resolution. Laplacian variance
// measures per-pixel detail DENSITY, and downsampling concentrates detail, so
// measuring a thumbnail natively inflated it enormously. Measured on one image
// at several sizes (before smooth() existed, so the absolute values are higher
// than the code produces today -- the ratio between the columns is the point):
//
//	           left native   scaled to 512
//	1600x1200        71.5            71.5
//	 640x480         72.2            72.2
//	 320x240        405.1            67.9
//	 160x120       2686.0            39.5
//
// A 160x120 thumbnail scored 37x its own source. The near-duplicate ranking
// consequently recommended keeping a 320x240 thumbnail over the 640x480
// original it came from.
//
// Scaling everything to a common grid fixes it at the root: the always-scaled
// column decreases monotonically as the image genuinely loses detail, which is
// what a sharpness metric should do. Upscaling a small image for MEASUREMENT
// invents no detail -- interpolation adds no high-frequency energy, so a
// thumbnail correctly scores below its source.
const analysisSize = 512

// Metrics is everything measured about one image. All scores are 0..1 unless
// noted, with higher meaning "more of the named property".
type Metrics struct {
	// Sharpness is normalised Laplacian variance. Higher means more
	// high-frequency detail, which usually means better focus.
	Sharpness float64 `json:"sharpness"`
	// RawLaplacianVariance is the unnormalised measurement, exposed because
	// the normalisation is a judgement call and this is not.
	RawLaplacianVariance float64 `json:"raw_laplacian_variance"`

	// Exposure is 1.0 for a well-distributed histogram, falling toward 0 as
	// the image becomes very dark or very bright.
	Exposure float64 `json:"exposure"`
	// MeanLuminance is the average brightness, 0..255.
	MeanLuminance float64 `json:"mean_luminance"`
	// ShadowClipping and HighlightClipping are the fraction of pixels crushed
	// to pure black or blown to pure white.
	ShadowClipping    float64 `json:"shadow_clipping"`
	HighlightClipping float64 `json:"highlight_clipping"`

	// Contrast is the normalised spread of the luminance histogram.
	Contrast float64 `json:"contrast"`
	// RawStdDev is the unnormalised standard deviation of luminance, 0..127.5.
	RawStdDev float64 `json:"raw_std_dev"`

	// Resolution scores pixel count against a reference, capped at 1.
	Resolution float64 `json:"resolution"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`

	// Overall combines the above. See Thresholds.Overall for the weighting and
	// why it is a weighted mean rather than a product.
	Overall float64 `json:"overall"`

	// Flags names each threshold this image fell below. Every flag is hedged:
	// these are candidates for review, not verdicts.
	Flags []string `json:"flags"`
}

// Flag names. Each is deliberately prefixed "possibly": the measurement is
// certain, the judgement is not.
const (
	FlagPossiblyBlurry       = "possibly_blurry"
	FlagPossiblyUnderexposed = "possibly_underexposed"
	FlagPossiblyOverexposed  = "possibly_overexposed"
	FlagPossiblyLowContrast  = "possibly_low_contrast"
	FlagLowResolution        = "low_resolution"
	FlagShadowsClipped       = "shadows_clipped"
	FlagHighlightsClipped    = "highlights_clipped"
)

// Thresholds are the configurable cut-offs at which a flag is raised.
//
// Every one of these is a judgement call rather than a fact about images,
// which is why they are configuration and not constants, and why the raw
// measurements are reported alongside the flags.
type Thresholds struct {
	// Sharpness below this raises FlagPossiblyBlurry.
	Sharpness float64
	// MeanLuminance outside these bounds raises an exposure flag.
	DarkLuminance   float64
	BrightLuminance float64
	// Clipping fraction above this raises a clipping flag.
	Clipping float64
	// Contrast below this raises FlagPossiblyLowContrast.
	Contrast float64
	// Pixels below this raises FlagLowResolution.
	MinPixels int64
}

// DefaultThresholds are calibrated against the fixture corpus. See
// DESIGN_DECISIONS.md for how, and for why they should be re-checked against a
// real photo library before anyone relies on them.
func DefaultThresholds() Thresholds {
	return Thresholds{
		// Measured across all 39 analysable fixtures, after the pre-smooth:
		//
		//   deliberately blurred   sharpness 0.020 .. 0.047   (3 photos)
		//   everything else                  0.200 .. 0.591   (36 photos)
		//
		// 0.12 sits near the centre of that gap. 0.15 also separates the
		// corpus cleanly but leaves only 0.05 of headroom above and 0.10
		// below; a threshold pressed against the sharp end is the one more
		// likely to start calling real photographs blurry, and on a real
		// library the low end of "everything else" is where the soft-focus
		// and shallow-depth-of-field frames live.
		//
		// The gap was much narrower before smooth() existed: sensor-level
		// noise in the blurred frames was propping their variance up.
		Sharpness: 0.12,
		// Measured against the fixture corpus:
		//
		//   normally exposed   mean luminance  77.9 .. 129.6
		//   deliberately dark                  19.2 ..  30.9
		//   deliberately bright               197.4 .. 217.1
		//
		// 46 and 190 sit in both gaps with room to spare. An earlier value of
		// 210 for the bright bound missed a genuinely overexposed frame at
		// 197.4, which is what measuring rather than guessing caught.
		//
		// The band is deliberately wide. A correctly exposed night photograph
		// IS dark and a correctly exposed snow scene IS bright; flagging
		// either would be a statement about taste rather than about pixels.
		// The asymmetry (46 from black, 65 from white) reflects that blown
		// highlights destroy detail irrecoverably while shadows usually retain
		// some.
		DarkLuminance:   46,
		BrightLuminance: 190,
		// 5% of pixels crushed or blown. Some clipping is normal and often
		// intentional -- a specular highlight, a black background.
		Clipping:  0.05,
		Contrast:  0.10,
		MinPixels: 640 * 480,
	}
}

// referencePixels is the resolution at which Resolution scores 1.0. Two
// megapixels is enough for a full-screen view and a good print at typical
// sizes; beyond that, more pixels do not make a photo more useful to keep.
const referencePixels = 2_000_000

// Analyze measures an image.
//
// The image is scaled to a fixed analysis size first, so scores are comparable
// across a library shot at different resolutions -- see analysisSize.
func Analyze(img image.Image, t Thresholds) Metrics {
	bounds := img.Bounds()
	m := Metrics{Width: bounds.Dx(), Height: bounds.Dy()}

	if m.Width <= 0 || m.Height <= 0 {
		return m
	}

	grey := toGreyscale(scaleForAnalysis(img))

	m.RawLaplacianVariance = laplacianVariance(smooth(grey))
	m.Sharpness = normaliseSharpness(m.RawLaplacianVariance)

	lum := luminanceStats(grey)
	m.MeanLuminance = lum.mean
	m.RawStdDev = lum.stdDev
	m.ShadowClipping = lum.shadowClipped
	m.HighlightClipping = lum.highlightClipped

	m.Exposure = scoreExposure(lum, t)
	m.Contrast = normaliseContrast(lum.stdDev)
	m.Resolution = scoreResolution(int64(m.Width) * int64(m.Height))

	m.Overall = overallScore(m)
	m.Flags = flagsFor(m, t)
	return m
}

// greyImage is a luminance plane at analysis resolution.
type greyImage struct {
	w, h int
	px   []float64 // 0..255
}

func (g *greyImage) at(x, y int) float64 { return g.px[y*g.w+x] }

// scaleForAnalysis resamples to a fixed long edge, preserving aspect ratio.
//
// Applied in BOTH directions. Leaving small images alone seems conservative
// and is not: it measures them on a denser grid than everything else and
// inflates their apparent detail. See analysisSize for the numbers.
func scaleForAnalysis(img image.Image) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}

	if w >= h {
		h = int(float64(h) * float64(analysisSize) / float64(w))
		w = analysisSize
	} else {
		w = int(float64(w) * float64(analysisSize) / float64(h))
		h = analysisSize
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom rather than ApproxBiLinear: a sharper kernel preserves more
	// of the high-frequency detail the Laplacian is about to measure, so the
	// downsample itself does not blur the thing being tested for blur.
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

func toGreyscale(img *image.RGBA) *greyImage {
	b := img.Bounds()
	g := &greyImage{w: b.Dx(), h: b.Dy()}
	g.px = make([]float64, g.w*g.h)

	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			c := img.RGBAAt(b.Min.X+x, b.Min.Y+y)
			// Rec. 601 luma. Perceptual weighting matters: a saturated blue
			// and a mid grey are not equally bright to a viewer, and treating
			// them as such would misreport both exposure and contrast.
			g.px[y*g.w+x] = 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
		}
	}
	return g
}

// smooth applies a separable 1-2-1 binomial kernel (a discrete Gaussian,
// sigma ~= 0.85) before the Laplacian runs.
//
// THIS IS NOT A SPEED OR NOISE TWEAK. It is what stops the metric from
// mistaking JPEG compression damage for detail.
//
// A Laplacian responds to any sharp intensity change. Lossy JPEG quantisation
// produces plenty of those which are not in the photograph: 8x8 block edges
// and ringing around contours. The operator cannot tell artifact energy from
// subject detail, so a heavily recompressed file measures as SHARPER than the
// original it was made from. Measured on three fixture pairs, same scene, same
// dimensions, the re-encode being a strictly degraded copy:
//
//	                     no pre-smooth        with pre-smooth
//	scene02  original           57.70                  35.90
//	         recompressed       75.49  (+31%)          38.22  (+6.5%)
//	scene10  original          104.56                  68.68
//	         recompressed      124.78  (+19%)          70.67  (+2.9%)
//	scene18  original           88.31                  57.70
//	         recompressed      107.07  (+21%)          59.42  (+3.0%)
//
// Blocking artifacts live at the very top of the frequency range, so a mild
// low-pass removes most of them while leaving real edges -- which span several
// pixels -- largely intact. The residual few percent is small enough to fall
// inside the near-duplicate ranker's quality tolerance, so file size decides
// instead and the original is kept.
//
// It also sharpens the distinction it is named for. On the five fixtures
// measured both ways, the worst-case ratio between the least-blurred blurred
// frame and the least-sharp sharp one went from 24x (66.79 / 2.79) to 57x
// (38.69 / 0.68): the smoothing removes sensor-level noise that was propping
// up the floor of the blurred images.
func smooth(g *greyImage) *greyImage {
	if g.w < 3 || g.h < 3 {
		return g
	}

	// Separable: two 1-D passes are O(6n) instead of the O(9n) a 3x3
	// convolution would cost, and identical in result.
	tmp := &greyImage{w: g.w, h: g.h, px: make([]float64, len(g.px))}
	out := &greyImage{w: g.w, h: g.h, px: make([]float64, len(g.px))}

	// Edges clamp to the nearest pixel rather than wrapping or zero-padding.
	// Zero padding would manufacture a hard black border and hand the
	// Laplacian a frame-wide artificial edge.
	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			l, r := max(x-1, 0), min(x+1, g.w-1)
			tmp.px[y*g.w+x] = (g.at(l, y) + 2*g.at(x, y) + g.at(r, y)) / 4
		}
	}
	for y := 0; y < g.h; y++ {
		u, d := max(y-1, 0), min(y+1, g.h-1)
		for x := 0; x < g.w; x++ {
			out.px[y*g.w+x] = (tmp.at(x, u) + 2*tmp.at(x, y) + tmp.at(x, d)) / 4
		}
	}
	return out
}

// laplacianVariance measures high-frequency energy: the classic blur metric.
//
// The 4-neighbour Laplacian kernel
//
//	0  1  0
//	1 -4  1
//	0  1  0
//
// responds to second-order intensity change -- edges and fine texture. A sharp
// image has many strong responses and therefore high variance; a blurred one
// has been low-pass filtered, so the responses collapse toward zero and the
// variance with them.
//
// VARIANCE, not mean absolute response, because variance is what distinguishes
// "detail everywhere" from "one bright edge in an otherwise flat frame".
//
// Borders are skipped rather than padded: padding invents an edge at the frame
// boundary and inflates the score for every image equally.
func laplacianVariance(g *greyImage) float64 {
	if g.w < 3 || g.h < 3 {
		return 0
	}

	// TWO-PASS, not the sum-of-squares shortcut.
	//
	// variance = sumSq/n - mean^2 is algebraically correct and numerically
	// treacherous: when the mean is large relative to the spread, it subtracts
	// two nearly-equal large numbers and the result is dominated by rounding.
	// On a uniform image it returned ~1e-8 instead of 0, which is invisible in
	// a formatted score and still fails an equality check -- and on a bright
	// flat image it can go negative outright.
	//
	// Two passes over an in-memory slice cost almost nothing here and the
	// result is exact for the degenerate cases that matter most.
	n := 0
	var sum float64
	response := make([]float64, 0, (g.w-2)*(g.h-2))

	for y := 1; y < g.h-1; y++ {
		for x := 1; x < g.w-1; x++ {
			v := g.at(x, y-1) + g.at(x, y+1) + g.at(x-1, y) + g.at(x+1, y) - 4*g.at(x, y)
			response = append(response, v)
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0
	}

	mean := sum / float64(n)
	var sumSqDiff float64
	for _, v := range response {
		d := v - mean
		sumSqDiff += d * d
	}
	return clampNoise(sumSqDiff / float64(n))
}

// noiseFloor is the level below which a measured variance is arithmetic
// residue rather than signal.
//
// The input is 8-bit integers, so the smallest real difference between two
// pixels is 1. Anything many orders of magnitude below that cannot be
// information about the image. It IS reachable in floating point: averaging
// 40,000 copies of an identical value produces a mean that differs from each
// value in the last bits, so a perfectly uniform image measured a standard
// deviation of 1.4e-14 rather than zero -- invisible in any formatted output
// and still not equal to zero, which is the worst combination.
const noiseFloor = 1e-9

func clampNoise(v float64) float64 {
	if v < noiseFloor {
		return 0
	}
	return v
}

// sharpnessSaturation is the raw Laplacian variance treated as "definitely
// sharp". Above it, more detail does not make a photo more in-focus.
//
// Calibrated on the fixture corpus, where a 6px box blur drops variance by
// roughly two orders of magnitude. Documented as approximate because it
// depends on subject matter: a photo of a brick wall out-scores a portrait
// against a plain backdrop without being better focused.
//
// Lowered from 500 when the pre-smooth in smooth() was introduced. That pass
// removes the top of the frequency range, which cost in-focus fixtures roughly
// 38% of their variance; holding the saturation constant would have dragged
// every score down without changing any ordering, and made the numbers stored
// before and after the change incomparable.
const sharpnessSaturation = 310.0

// normaliseSharpness maps unbounded variance onto 0..1.
//
// A square-root curve rather than linear: the perceptual difference between
// variance 5 and 50 is enormous (unusable vs acceptable) while 500 and 5000
// both just read as "sharp". A linear map would compress the range that
// actually matters into the bottom few percent.
func normaliseSharpness(v float64) float64 {
	if v <= 0 {
		return 0
	}
	s := math.Sqrt(v / sharpnessSaturation)
	if s > 1 {
		return 1
	}
	return s
}

type lumStats struct {
	mean             float64
	stdDev           float64
	shadowClipped    float64
	highlightClipped float64
}

// luminanceStats computes brightness distribution in a single pass.
func luminanceStats(g *greyImage) lumStats {
	if len(g.px) == 0 {
		return lumStats{}
	}

	var sum float64
	var dark, bright int

	for _, v := range g.px {
		sum += v
		// "Clipped" means at or adjacent to the extreme, since JPEG
		// quantisation rarely lands exactly on 0 or 255.
		if v <= 2 {
			dark++
		}
		if v >= 253 {
			bright++
		}
	}

	n := float64(len(g.px))
	mean := sum / n

	// Two-pass, for the same numerical reason as laplacianVariance. Luminance
	// values sit around 128 with a spread of maybe 20, so the shortcut formula
	// subtracts two numbers that agree to four significant figures.
	var sumSqDiff float64
	for _, v := range g.px {
		d := v - mean
		sumSqDiff += d * d
	}

	return lumStats{
		mean:             mean,
		stdDev:           math.Sqrt(clampNoise(sumSqDiff / n)),
		shadowClipped:    float64(dark) / n,
		highlightClipped: float64(bright) / n,
	}
}

// scoreExposure rates how well the tones are distributed.
//
// Deliberately forgiving in the middle and only penalising the extremes. A
// correctly exposed night photograph IS dark and a correctly exposed snow
// scene IS bright; treating either as a defect would be a statement about
// taste rather than about the pixels.
//
// What it does penalise is information that has been destroyed: pixels crushed
// to pure black or blown to pure white cannot be recovered by any edit, which
// is an objective loss rather than an aesthetic one.
func scoreExposure(l lumStats, t Thresholds) float64 {
	score := 1.0

	// Distance outside the acceptable band, scaled so that reaching pure black
	// or pure white costs the full point.
	if l.mean < t.DarkLuminance {
		score -= (t.DarkLuminance - l.mean) / t.DarkLuminance
	} else if l.mean > t.BrightLuminance {
		score -= (l.mean - t.BrightLuminance) / (255 - t.BrightLuminance)
	}

	// Clipping is a separate, additive penalty: an image can sit at a
	// reasonable mean while still having blown highlights.
	score -= l.shadowClipped * 0.5
	score -= l.highlightClipped * 0.5

	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// contrastSaturation is the luminance standard deviation treated as full
// contrast. The theoretical maximum is 127.5 (half the pixels black, half
// white), which no real photograph approaches; 64 is a well-separated image.
const contrastSaturation = 64.0

func normaliseContrast(stdDev float64) float64 {
	c := stdDev / contrastSaturation
	if c > 1 {
		return 1
	}
	if c < 0 {
		return 0
	}
	return c
}

func scoreResolution(pixels int64) float64 {
	if pixels <= 0 {
		return 0
	}
	r := float64(pixels) / float64(referencePixels)
	if r > 1 {
		return 1
	}
	return r
}

// overallScore combines the metrics into one number.
//
// A WEIGHTED MEAN, not a product. A product would let any single low score
// drag the total to near zero, so a correctly-exposed sharp photo of a foggy
// landscape -- genuinely low contrast -- would score as badly as an unusable
// one. The mean lets strengths offset weaknesses, which matches how a person
// would judge it.
//
// Sharpness carries the most weight because it is the least recoverable: an
// under-exposed photo can be lifted in an editor, a low-contrast one can be
// stretched, but focus that was never captured cannot be restored.
//
// This number is deliberately NOT presented as "photo quality" anywhere in the
// UI. It orders candidates within a group of near-duplicates, which is a
// comparison between versions of the same picture -- a question it can
// actually answer.
func overallScore(m Metrics) float64 {
	const (
		wSharpness  = 0.45
		wExposure   = 0.30
		wContrast   = 0.15
		wResolution = 0.10
	)
	s := m.Sharpness*wSharpness +
		m.Exposure*wExposure +
		m.Contrast*wContrast +
		m.Resolution*wResolution

	if s < 0 {
		return 0
	}
	if s > 1 {
		return 1
	}
	return s
}

func flagsFor(m Metrics, t Thresholds) []string {
	// Non-nil so the API renders [] rather than null.
	flags := []string{}

	if m.Sharpness < t.Sharpness {
		flags = append(flags, FlagPossiblyBlurry)
	}
	if m.MeanLuminance < t.DarkLuminance {
		flags = append(flags, FlagPossiblyUnderexposed)
	}
	if m.MeanLuminance > t.BrightLuminance {
		flags = append(flags, FlagPossiblyOverexposed)
	}
	if m.Contrast < t.Contrast {
		flags = append(flags, FlagPossiblyLowContrast)
	}
	if int64(m.Width)*int64(m.Height) < t.MinPixels {
		flags = append(flags, FlagLowResolution)
	}
	// Clipping flags are separate from exposure flags: a photo can be
	// acceptably exposed overall and still have destroyed detail at one end.
	if m.ShadowClipping > t.Clipping {
		flags = append(flags, FlagShadowsClipped)
	}
	if m.HighlightClipping > t.Clipping {
		flags = append(flags, FlagHighlightsClipped)
	}
	return flags
}
