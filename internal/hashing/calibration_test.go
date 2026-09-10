package hashing

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"testing"

	"golang.org/x/image/draw"
)

// TestCalibrateThreshold measures the separation between near-duplicate and
// unrelated distances on two styles of synthetic image, so the similarity
// threshold is chosen from data rather than guessed.
//
// Run with: go test -run TestCalibrateThreshold -v ./internal/hashing/
func TestCalibrateThreshold(t *testing.T) {
	for _, style := range []struct {
		name string
		gen  func(int64, int, int) image.Image
	}{
		{"fixture (hard edges + fine stripes)", calibFixtureStyle},
		{"photo-like (smooth low-frequency)", calibPhotoStyle},
	} {
		const n = 30
		nearMax, nearSum, nearCount := 0, 0, 0
		unrelSum, unrelMin, unrelCount := 0, 64, 0
		hashes := make([]uint64, n)

		for i := 0; i < n; i++ {
			orig := style.gen(int64(i+1), 640, 480)
			hOrig := calibHash(t, calibEncode(orig, 92))
			hRecomp := calibHash(t, calibEncode(orig, 40))
			hSmall := calibHash(t, calibEncode(calibResize(orig, 320, 240), 85))
			hashes[i] = hOrig

			for _, d := range []int{
				HammingDistance(hOrig, hRecomp),
				HammingDistance(hOrig, hSmall),
				HammingDistance(hRecomp, hSmall),
			} {
				if d > nearMax {
					nearMax = d
				}
				nearSum += d
				nearCount++
			}
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				d := HammingDistance(hashes[i], hashes[j])
				if d < unrelMin {
					unrelMin = d
				}
				unrelSum += d
				unrelCount++
			}
		}

		t.Logf("%-38s near-dupe mean %4.1f max %2d  |  unrelated mean %4.1f min %2d  |  separation %d",
			style.name,
			float64(nearSum)/float64(nearCount), nearMax,
			float64(unrelSum)/float64(unrelCount), unrelMin,
			unrelMin-nearMax)
	}
}

func calibHash(t *testing.T, data []byte) uint64 {
	t.Helper()
	h, err := DHashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func calibEncode(img image.Image, q int) []byte {
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: q})
	return b.Bytes()
}

func calibResize(img image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
	return dst
}

// calibFixtureStyle reproduces the current corpus generator: hard-edged
// rectangles plus a fine periodic diagonal stripe pattern.
func calibFixtureStyle(seed int64, w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := uint64(seed)*6364136223846793005 + 1442695040888963407
	next := func() int { s = s*6364136223846793005 + 1442695040888963407; return int(s >> 40 & 0xFF) }

	bg := color.RGBA{uint8(60 + next()%120), uint8(60 + next()%120), uint8(60 + next()%120), 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	for i := 0; i < 14; i++ {
		x0, y0 := next()%w, next()%h
		rw, rh := 20+next()%140, 20+next()%110
		c := color.RGBA{uint8(next()), uint8(next()), uint8(next()), 255}
		draw.Draw(img, image.Rect(x0, y0, x0+rw, y0+rh).Intersect(img.Bounds()),
			&image.Uniform{c}, image.Point{}, draw.Src)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%64 < 6 {
				v := uint8(x * 255 / w)
				img.SetRGBA(x, y, color.RGBA{v, 255 - v, uint8(y * 255 / h), 255})
			}
		}
	}
	return img
}

// calibPhotoStyle builds smooth low-frequency luminance structure, which is
// what real photographs actually look like at the 9x8 scale a perceptual hash
// samples.
func calibPhotoStyle(seed int64, w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := uint64(seed)*6364136223846793005 + 1442695040888963407
	nf := func() float64 {
		s = s*6364136223846793005 + 1442695040888963407
		return float64(s>>40&0xFFFF) / 65535.0
	}

	type wave struct{ ax, ay, phase, amp float64 }
	waves := make([]wave, 4)
	for i := range waves {
		waves[i] = wave{
			ax:    (nf()*2 - 1) * 2.5,
			ay:    (nf()*2 - 1) * 2.5,
			phase: nf() * 6.283,
			amp:   0.3 + nf()*0.7,
		}
	}
	baseR, baseG, baseB := nf()*80+60, nf()*80+60, nf()*80+60

	clamp := func(f float64) uint8 {
		if f < 0 {
			return 0
		}
		if f > 255 {
			return 255
		}
		return uint8(f)
	}

	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			v := 0.0
			for _, wv := range waves {
				v += wv.amp * math.Sin(wv.ax*fx*6.283+wv.ay*fy*6.283+wv.phase)
			}
			v = v / 4.0 * 90
			img.SetRGBA(x, y, color.RGBA{clamp(baseR + v), clamp(baseG + v*0.8), clamp(baseB + v*1.2), 255})
		}
	}
	_ = fmt.Sprint
	return img
}

// TestCalibrateBitCoverage measures how many of the 64 bit positions are ever
// set, across sample counts, to distinguish a structural bias in the hash from
// simply not having sampled enough images.
func TestCalibrateBitCoverage(t *testing.T) {
	for _, n := range []int{40, 400} {
		var union, intersection uint64
		intersection = ^uint64(0)
		for i := 1; i <= n; i++ {
			h := DHash(noiseImage(120, 90, int64(i)))
			union |= h
			intersection &= h
		}
		t.Logf("noise   n=%4d  positions ever set: %2d/64   always set: %2d",
			n, popcount(union), popcount(intersection))
	}

	for _, n := range []int{40} {
		var union uint64
		for i := 1; i <= n; i++ {
			union |= DHash(calibPhotoStyle(int64(i), 640, 480))
		}
		t.Logf("photo   n=%4d  positions ever set: %2d/64", n, popcount(union))
	}
}

func popcount(v uint64) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
}

// noiseImage builds deterministic blocky pseudo-random images. Kept only for
// the bit-coverage calibration above, which demonstrates that its 16px blocks
// alias against the 9x8 hash grid -- a worked example of why fixture imagery
// has to resemble photographs.
func noiseImage(w, h int, seed int64) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	s := uint64(seed)*6364136223846793005 + 1442695040888963407
	next := func() uint8 {
		s = s*6364136223846793005 + 1442695040888963407
		return uint8(s >> 56)
	}
	block := 16
	for by := 0; by < h; by += block {
		for bx := 0; bx < w; bx += block {
			v := next()
			for y := by; y < by+block && y < h; y++ {
				for x := bx; x < bx+block && x < w; x++ {
					img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
				}
			}
		}
	}
	return img
}
