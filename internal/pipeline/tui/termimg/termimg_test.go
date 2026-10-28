package termimg

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestDetect(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want Mode
	}{
		{map[string]string{"TERM": "xterm-kitty"}, Kitty},
		{map[string]string{"KITTY_WINDOW_ID": "1", "TERM": "xterm-256color"}, Kitty},
		{map[string]string{"TERM_PROGRAM": "ghostty", "TERM": "xterm-ghostty"}, Kitty},
		{map[string]string{"TERM_PROGRAM": "ghostty", "TMUX": "/tmp/tmux-1/default,1,0"}, Link},
		{map[string]string{"TERM_PROGRAM": "iTerm.app"}, Link},
		{map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, Link},
		{map[string]string{"TERM": "xterm-kitty", "YMCP_IMAGES": "off"}, Link},
		{map[string]string{"TERM_PROGRAM": "WezTerm", "YMCP_IMAGES": "kitty"}, Kitty},
		{map[string]string{}, Link},
	} {
		if got := Detect(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("Detect(%v) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestFit(t *testing.T) {
	for _, c := range []struct{ w, h, mc, mr, cols, rows int }{
		{48, 48, 80, 16, 6, 3},       // small: natural size
		{1920, 1080, 80, 16, 56, 16}, // a screenshot, as tall as it may be
		{1920, 400, 80, 16, 80, 9},   // wide: as wide as it may be
		{10, 10, 80, 16, 2, 1},
		{0, 10, 80, 16, 0, 0},
	} {
		if cols, rows := Fit(c.w, c.h, c.mc, c.mr); cols != c.cols || rows != c.rows {
			t.Errorf("Fit(%d, %d, %d, %d) = %d×%d, want %d×%d", c.w, c.h, c.mc, c.mr, cols, rows, c.cols, c.rows)
		}
	}
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// gifOf is a GIF of n frames, each a w×h square drawn over the last.
func gifOf(t *testing.T, w, h, n int) []byte {
	t.Helper()
	pal := color.Palette{color.Transparent, color.White, color.Black}
	g := &gif.GIF{Config: image.Config{Width: w, Height: h, ColorModel: pal}}
	for i := range n {
		fr := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		fr.SetColorIndex(i%w, 0, uint8(1+i%2))
		g.Image = append(g.Image, fr)
		g.Delay = append(g.Delay, 5)
		g.Disposal = append(g.Disposal, gif.DisposalNone)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecode(t *testing.T) {
	p, err := Decode(pngOf(t, 30, 20))
	if err != nil || p.W != 30 || p.H != 20 || len(p.Frames) != 1 {
		t.Fatalf("png: %+v, %v", p, err)
	}
	p, err = Decode(gifOf(t, 8, 8, 3))
	if err != nil || len(p.Frames) != 3 || len(p.Delays) != 3 || p.Delays[0].Milliseconds() != 50 {
		t.Fatalf("gif: %+v, %v", p, err)
	}
	// Frames add up: the first frame's pixel is still there in the last.
	if _, _, _, a := p.Frames[2].At(0, 0).RGBA(); a == 0 {
		t.Error("the frames were not composed")
	}
	if _, err := Decode([]byte("not a picture")); err == nil {
		t.Error("garbage decoded")
	}
}

func TestPlace(t *testing.T) {
	p, err := Decode(pngOf(t, 400, 300))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := Place(p, ID("https://x/a.png", 20, 5), 20, 5)
	if err != nil {
		t.Fatal(err)
	}
	lines := pl.Lines()
	if len(lines) != 5 {
		t.Fatalf("rows = %d", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 20 {
			t.Errorf("row %d is %d cells wide", i, w)
		}
	}
	if !strings.HasPrefix(lines[0], "\x1b_Ga=T,U=1,f=100,i=") || strings.Contains(lines[1], "\x1b_G") {
		t.Errorf("the picture is not sent before the first row only: %q", lines[0][:40])
	}
	if strings.Contains(pl.Send, "a=f") {
		t.Error("a still picture sent as an animation")
	}

	anim, err := Decode(gifOf(t, 48, 48, 4))
	if err != nil {
		t.Fatal(err)
	}
	pl, err = Place(anim, 7, 6, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(pl.Send, "a=f,") != 3 || !strings.Contains(pl.Send, "a=a,i=7,s=3,v=1") {
		t.Errorf("animation: %q", pl.Send)
	}
}

func TestChunked(t *testing.T) {
	s := chunked(bytes.Repeat([]byte{1}, 10000), "a=T", "i=1")
	if n := strings.Count(s, "\x1b_G"); n != 4 {
		t.Errorf("chunks = %d", n)
	}
	if !strings.HasPrefix(s, "\x1b_Ga=T,i=1,m=1;") || !strings.Contains(s, "\x1b_Gm=0,q=2;") {
		t.Errorf("chunk keys: %q…", s[:30])
	}
}
