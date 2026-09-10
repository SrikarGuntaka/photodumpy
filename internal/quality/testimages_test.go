package quality

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/draw"
)

// solidImage is a uniform colour: no detail, no contrast. The degenerate case.
func solidImage(w, h int, v uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{v, v, v, 255}}, image.Point{}, draw.Src)
	return img
}

// detailedImage builds smooth structure plus genuine fine texture -- the same
// two-layer shape the fixture corpus uses, and for the same reason: a purely
// smooth image has near-zero Laplacian variance and would read as blurry no
// matter how well focused it was.
func detailedImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

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

			// Low-frequency base.
			low := 60*math.Sin(2.1*fx*2*math.Pi+0.4) + 45*math.Sin(1.7*fy*2*math.Pi+1.1)

			// Fine, non-periodic texture. Incommensurable frequencies so it
			// never forms a repeating pattern that could alias.
			fine := 22*math.Sin(37.3*fx*2*math.Pi) +
				18*math.Sin(29.7*fy*2*math.Pi+0.9) +
				14*math.Sin(43.1*(fx+fy)*2*math.Pi+2.2)

			v := 128 + low + fine
			img.SetRGBA(x, y, color.RGBA{clamp(v), clamp(v * 0.95), clamp(v * 1.05), 255})
		}
	}
	return img
}

// boxBlur applies a separable box blur, genuinely destroying high-frequency
// detail rather than merely smoothing the appearance.
func boxBlur(src image.Image, radius int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if radius < 1 || w == 0 || h == 0 {
		return src
	}

	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	tmp := image.NewRGBA(image.Rect(0, 0, w, h))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, n int
			for dx := -radius; dx <= radius; dx++ {
				sx := x + dx
				if sx < 0 || sx >= w {
					continue
				}
				c := rgba.RGBAAt(sx, y)
				r += int(c.R)
				g += int(c.G)
				bl += int(c.B)
				n++
			}
			tmp.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, n int
			for dy := -radius; dy <= radius; dy++ {
				sy := y + dy
				if sy < 0 || sy >= h {
					continue
				}
				c := tmp.RGBAAt(x, sy)
				r += int(c.R)
				g += int(c.G)
				bl += int(c.B)
				n++
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return dst
}

func scaleTo(src image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	return dst
}
