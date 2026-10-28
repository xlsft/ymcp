package termimg

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	_ "image/jpeg" // screenshots
	_ "image/png"
	"time"
)

// Picture is a decoded picture: one frame, or the frames of an animated
// GIF, each a whole image.
type Picture struct {
	Frames []image.Image
	Delays []time.Duration // per frame, for an animation
	W, H   int
}

// MaxBytes is the largest file Decode reads.
const MaxBytes = 20 << 20

// Decode reads a PNG, JPEG or GIF. The frames of an animated GIF are drawn
// over each other as the GIF says, so each frame is a whole picture.
func Decode(data []byte) (*Picture, error) {
	if len(data) > MaxBytes {
		return nil, errors.New("the picture is too large")
	}
	if bytes.HasPrefix(data, []byte("GIF8")) {
		g, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return gifPicture(g), nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	return &Picture{Frames: []image.Image{img}, W: b.Dx(), H: b.Dy()}, nil
}

// gifPicture composes the frames of g onto its canvas.
func gifPicture(g *gif.GIF) *Picture {
	w, h := g.Config.Width, g.Config.Height
	if w == 0 || h == 0 {
		b := g.Image[0].Bounds()
		w, h = b.Max.X, b.Max.Y
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	p := &Picture{W: w, H: h}
	for i, fr := range g.Image {
		var saved *image.RGBA
		disposal := byte(0)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		if disposal == gif.DisposalPrevious {
			saved = image.NewRGBA(canvas.Bounds())
			copy(saved.Pix, canvas.Pix)
		}
		draw.Draw(canvas, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
		out := image.NewRGBA(canvas.Bounds())
		copy(out.Pix, canvas.Pix)
		p.Frames = append(p.Frames, out)
		delay := 100 * time.Millisecond
		if i < len(g.Delay) && g.Delay[i] > 0 {
			delay = time.Duration(g.Delay[i]) * 10 * time.Millisecond
		}
		p.Delays = append(p.Delays, delay)
		switch disposal {
		case gif.DisposalBackground:
			draw.Draw(canvas, fr.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			canvas = saved
		}
	}
	return p
}

// Cell is the assumed size of a terminal cell in pixels; terminals don't
// say, and the picture is fitted into its cells anyway.
const (
	CellW = 9
	CellH = 18
)

// Fit is the size in cells of a picture of w×h pixels at most maxCols wide
// and maxRows tall, at its natural size when that is smaller.
func Fit(w, h, maxCols, maxRows int) (cols, rows int) {
	if w <= 0 || h <= 0 || maxCols <= 0 || maxRows <= 0 {
		return 0, 0
	}
	cols = min(maxCols, max((w+CellW-1)/CellW, 1))
	rows = max((cols*CellW*h/w+CellH-1)/CellH, 1)
	if rows > maxRows {
		rows = maxRows
		cols = max(min(rows*CellH*w/(h*CellW), maxCols), 1)
	}
	return cols, rows
}

// scale shrinks img to fit w×h pixels, keeping its proportions, by
// averaging the pixels each new one covers; it never enlarges.
func scale(img image.Image, w, h int) image.Image {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= w && sh <= h {
		return img
	}
	f := max(float64(sw)/float64(w), float64(sh)/float64(h))
	dw, dh := max(int(float64(sw)/f), 1), max(int(float64(sh)/f), 1)
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		y0, y1 := b.Min.Y+y*sh/dh, b.Min.Y+max((y+1)*sh/dh, y*sh/dh+1)
		for x := range dw {
			x0, x1 := b.Min.X+x*sw/dw, b.Min.X+max((x+1)*sw/dw, x*sw/dw+1)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					c := color.NRGBA64Model.Convert(img.At(xx, yy)).(color.NRGBA64)
					r, g, bl, a = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A)
					n++
				}
			}
			out.Set(x, y, color.NRGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: uint16(a / n)})
		}
	}
	return out
}
