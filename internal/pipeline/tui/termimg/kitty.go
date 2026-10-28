package termimg

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"image/png"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// Placement is a picture ready for the kitty protocol in Cols×Rows cells:
// Send transmits it, and Lines are the cells that show it.
type Placement struct {
	ID         uint32
	Cols, Rows int
	Send       string
}

// ID is the image id of a picture shown at a size: kitty keeps images by
// id, and the placeholder cells carry it in their colour, 24 bits of it.
func ID(key string, cols, rows int) uint32 {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s|%d|%d", key, cols, rows)
	return h.Sum32()&0xFFFFFF | 1
}

// Place prepares p for cols×rows cells under id. The picture is shrunk to
// about the pixels those cells hold before it is sent. An animated GIF
// goes out as frames that the terminal plays in a loop itself, so the
// cells don't change from frame to frame.
func Place(p *Picture, id uint32, cols, rows int) (Placement, error) {
	if p == nil || len(p.Frames) == 0 || cols <= 0 || rows <= 0 {
		return Placement{}, fmt.Errorf("nothing to place")
	}
	pl := Placement{ID: id, Cols: cols, Rows: rows}
	var b strings.Builder
	sid := "i=" + strconv.FormatUint(uint64(id), 10)
	for i, fr := range p.Frames {
		var buf bytes.Buffer
		if err := png.Encode(&buf, scale(fr, cols*CellW, rows*CellH)); err != nil {
			return Placement{}, err
		}
		if i == 0 {
			b.WriteString(chunked(buf.Bytes(), "a=T", "U=1", "f=100", sid, "c="+strconv.Itoa(cols), "r="+strconv.Itoa(rows), "q=2"))
			continue
		}
		b.WriteString(chunked(buf.Bytes(), "a=f", "f=100", sid, "z="+strconv.FormatInt(p.Delays[i].Milliseconds(), 10), "q=2"))
	}
	if len(p.Frames) > 1 {
		// The first frame's delay, then play in a loop.
		b.WriteString(ansi.KittyGraphics(nil, "a=a", sid, "r=1", "z="+strconv.FormatInt(p.Delays[0].Milliseconds(), 10), "q=2"))
		b.WriteString(ansi.KittyGraphics(nil, "a=a", sid, "s=3", "v=1", "q=2"))
	}
	pl.Send = b.String()
	return pl, nil
}

// chunked is the graphics command with keys and data, split into the
// chunks the protocol takes.
func chunked(data []byte, keys ...string) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for first := true; first || enc != ""; first = false {
		n := min(len(enc), kitty.MaxChunkSize)
		part := enc[:n]
		enc = enc[n:]
		more := "m=0"
		if enc != "" {
			more = "m=1"
		}
		opts := []string{more, "q=2"}
		if first {
			opts = append(append([]string(nil), keys...), more)
		}
		b.WriteString(ansi.KittyGraphics([]byte(part), opts...))
	}
	return b.String()
}

// Lines are the placeholder rows of the picture: each row's first cell
// names its row and column with diacritics, the rest follow on, all in the
// colour that carries the image id. The first row starts with Send, so the
// terminal has the picture before it draws the cells, and gets it again
// whenever the row is redrawn.
func (pl Placement) Lines() []string {
	colour := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", pl.ID>>16&0xFF, pl.ID>>8&0xFF, pl.ID&0xFF)
	rest := strings.Repeat(string(kitty.Placeholder), max(pl.Cols-1, 0))
	out := make([]string, pl.Rows)
	for r := range pl.Rows {
		out[r] = colour + string(kitty.Placeholder) + string(kitty.Diacritic(r)) + string(kitty.Diacritic(0)) + rest + "\x1b[39m"
	}
	if len(out) > 0 {
		out[0] = pl.Send + out[0]
	}
	return out
}
