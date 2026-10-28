package views

import (
	"context"
	_ "embed"
	"net/url"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/xlsft/ymcp/internal/pipeline/text"
	"github.com/xlsft/ymcp/internal/pipeline/tui/termimg"
	"github.com/xlsft/ymcp/internal/pipeline/tui/theme"
)

// workingGIF is the loader of an agent's working placeholder, the same
// picture the placeholder links to, so it shows without a download.
//
//go:embed working.gif
var workingGIF []byte

// workingURL is the address of the loader in the placeholder.
var workingURL = func() string {
	m := mdImage.FindStringSubmatch(text.Working)
	return m[2]
}()

// Limits of the pictures a card shows.
const (
	pictureRows = 16 // the tallest a picture gets, in rows
	loaderRows  = 2  // the working loader
	maxPictures = 40 // the most pictures a card downloads
)

// pictures fetches the pictures of a card once each and draws them the
// way the terminal allows: in place with the kitty protocol, or as links.
type pictures struct {
	mode  termimg.Mode
	base  string       // the YouTrack URL, to make attachment links whole
	files []Attachment // the issue's attachments, which comments name too
	byURL map[string]*picture
}

// picture is one picture of a card, by its address.
type picture struct {
	pic     *termimg.Picture
	err     error
	loading bool
	placed  map[[2]int]termimg.Placement // by columns and rows
}

// picturesLoaded is a download that finished.
type picturesLoaded struct {
	to   int64
	url  string
	data []byte
	err  error
}

func newPictures(mode termimg.Mode, base string) *pictures {
	p := &pictures{mode: mode, base: strings.TrimRight(base, "/"), byURL: map[string]*picture{}}
	if mode == termimg.Kitty {
		pic, err := termimg.Decode(workingGIF)
		p.byURL[workingURL] = &picture{pic: pic, err: err}
	}
	return p
}

// link is the address of a picture to open in a browser: YouTrack's own
// addresses made whole.
func (p *pictures) link(src string) string {
	if strings.HasPrefix(src, "/") && p.base != "" {
		return p.base + src
	}
	return src
}

// fetch starts the downloads of the pictures at urls not asked for yet;
// none when the terminal shows links.
func (p *pictures) fetch(d *Deps, profile string, to int64, urls []string) tea.Cmd {
	if p.mode != termimg.Kitty || d.Pictures == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, u := range urls {
		if u == "" || p.byURL[u] != nil || len(p.byURL) >= maxPictures {
			continue
		}
		p.byURL[u] = &picture{loading: true}
		pics, u := d.Pictures, u
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), ytTimeout)
			defer cancel()
			data, err := pics.Fetch(ctx, profile, u)
			return picturesLoaded{to: to, url: u, data: data, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// loaded keeps a finished download.
func (p *pictures) loaded(msg picturesLoaded) {
	pc := p.byURL[msg.url]
	if pc == nil {
		return
	}
	pc.loading, pc.err = false, msg.err
	if msg.err == nil {
		pc.pic, pc.err = termimg.Decode(msg.data)
	}
}

// lines draws the picture at src, called alt, at most w cells wide: the
// picture itself once it is here and the terminal can show it, else a
// link to it.
func (p *pictures) lines(alt, src string, w, maxRows int) []string {
	if p != nil && p.mode == termimg.Kitty && p.byURL != nil {
		if pc := p.byURL[src]; pc != nil && pc.pic != nil {
			cols, rows := termimg.Fit(pc.pic.W, pc.pic.H, w, maxRows)
			if pc.placed == nil {
				pc.placed = map[[2]int]termimg.Placement{}
			}
			pl, ok := pc.placed[[2]int{cols, rows}]
			if !ok {
				var err error
				if pl, err = termimg.Place(pc.pic, termimg.ID(src, cols, rows), cols, rows); err == nil {
					pc.placed[[2]int{cols, rows}] = pl
					ok = true
				}
			}
			if ok {
				return pl.Lines()
			}
		} else if pc != nil && pc.loading {
			return []string{cut(theme.Dim.Render("loading "+pictureName(alt, src)+"…"), w)}
		}
	}
	return []string{p.linkLine("image", alt, src, w)}
}

// linkLine is a picture or another file as a link: what it is, its name,
// then its address, which terminals that know hyperlinks also open on a
// click.
func (p *pictures) linkLine(kind, alt, src string, w int) string {
	href := src
	if p != nil {
		href = p.link(src)
	}
	label := theme.Dim.Render(kind+" ") + pictureName(alt, src)
	if href == "" {
		return cut(label, w)
	}
	return cut(ansi.SetHyperlink(href)+label+theme.Dim.Render(" "+href)+ansi.ResetHyperlink(), w)
}

// pictureName is what a picture is called: its alt text, or the file name
// in its address.
func pictureName(alt, src string) string {
	if alt = strings.TrimSpace(alt); alt != "" {
		return alt
	}
	if u, err := url.Parse(src); err == nil && u.Path != "" {
		src = u.Path
	}
	if i := strings.LastIndex(src, "/"); i >= 0 {
		src = src[i+1:]
	}
	if src == "" {
		return "picture"
	}
	return src
}

// isPicture reports whether an attachment is a picture the card can show.
func isPicture(a Attachment) bool {
	if strings.HasPrefix(a.MimeType, "image/") {
		return true
	}
	n := strings.ToLower(a.Name)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

// resolve is the address of a picture a text names: an address as it is,
// or the name of one of the attachments, which YouTrack's markdown uses
// for pasted screenshots.
func resolve(src string, atts ...[]Attachment) string {
	src = strings.TrimSpace(src)
	if src == "" || strings.Contains(src, "://") || strings.HasPrefix(src, "/") {
		return src
	}
	name, _ := url.PathUnescape(src)
	for _, list := range atts {
		for _, a := range list {
			if a.Name == src || a.Name == name {
				return a.URL
			}
		}
	}
	return src
}

// pictureRefs are the addresses of the pictures in a markdown text, with
// names resolved against the attachments.
func pictureRefs(s string, atts ...[]Attachment) []string {
	var out []string
	for _, m := range mdImage.FindAllStringSubmatch(s, -1) {
		out = append(out, resolve(m[2], atts...))
	}
	return out
}

// file draws an attached file: a picture as one, anything else as a link.
func (p *pictures) file(f Attachment, w int) []string {
	if isPicture(f) {
		return p.lines(f.Name, f.URL, w, pictureRows)
	}
	return []string{p.linkLine("file", f.Name, f.URL, w)}
}
