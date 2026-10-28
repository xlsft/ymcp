package views

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/xlsft/ymcp/internal/pipeline/tui/termimg"
)

// fakePictures serves the issues' attachments and a picture for every
// address it knows.
type fakePictures struct {
	mu      sync.Mutex
	files   map[string][]Attachment // by issue
	data    map[string][]byte       // by address
	fetched []string
}

func (f *fakePictures) Attachments(_ context.Context, _, issue string) ([]Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[issue], nil
}

func (f *fakePictures) Fetch(_ context.Context, _, url string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, url)
	if d, ok := f.data[url]; ok {
		return d, nil
	}
	return nil, context.DeadlineExceeded
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// screenshotFixture: BE-127 has a screenshot attached to the issue and
// named in its description, and a comment that is only a pasted picture.
func screenshotFixture(t *testing.T, mode termimg.Mode) (*fixture, *fakePictures) {
	f := newFixture(t)
	yt := withIssues(f)
	is := yt.issues["2-127"]
	is.Description = "The export button:\n\n![](button.png){width=70%}\n\nThat's all."
	yt.issues["2-127"] = is
	yt.add("2-127", IssueComment{ID: "4-9", Author: "anna", AuthorName: "Anna Petrova", Created: t0,
		Files: []Attachment{{Name: "pasted.png", URL: "/api/files/9?sign=b", MimeType: "image/png"}}})
	pics := &fakePictures{
		files: map[string][]Attachment{"2-127": {
			{Name: "button.png", URL: "/api/files/1?sign=a", MimeType: "image/png"},
			{Name: "log.txt", URL: "/api/files/2?sign=c", MimeType: "text/plain"},
		}},
		data: map[string][]byte{"/api/files/1?sign=a": testPNG(t, 300, 120), "/api/files/9?sign=b": testPNG(t, 90, 36)},
	}
	f.d.Pictures, f.d.PictureMode = pics, mode
	return f, pics
}

// TestPicturesAsLinks: where the terminal can't show pictures, they are
// links, a comment that is only a picture included, and nothing is
// downloaded.
func TestPicturesAsLinks(t *testing.T) {
	f, pics := screenshotFixture(t, termimg.Link)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 160, 60)
	d.has("The export button:", "image button.png https://example.youtrack.cloud/api/files/1?sign=a", "That's all.",
		"file log.txt https://example.youtrack.cloud/api/files/2?sign=c",
		"image pasted.png https://example.youtrack.cloud/api/files/9?sign=b")
	d.hasNot("(empty)", "image button.png https://example.youtrack.cloud/api/files/1?sign=a  image")
	if len(pics.fetched) != 0 {
		t.Errorf("downloaded %v for links", pics.fetched)
	}
	if strings.Count(ansi.Strip(d.m.View()), "button.png") != 1 {
		t.Error("the screenshot shows twice: in the description and as an attachment")
	}
}

// TestPicturesInPlace: with the kitty protocol, pictures are downloaded
// once and drawn in place as placeholder cells; the agent's working
// placeholder is the loader.
func TestPicturesInPlace(t *testing.T) {
	f, pics := screenshotFixture(t, termimg.Kitty)
	d := drive(t, NewRun(&f.d, "backend", f.runs["BE-127"].Number, false), 160, 60)
	view := d.m.View()
	if n := strings.Count(view, string(kitty.Placeholder)); n == 0 {
		t.Fatalf("no picture drawn:\n%s", ansi.Strip(view))
	}
	if !strings.Contains(view, "\x1b_Ga=T,U=1,f=100,") {
		t.Error("the picture is not sent")
	}
	d.hasNot("image button.png", "image pasted.png", "(empty)")
	// The pasted picture is the whole comment: a blank line under its header.
	lines, found := strings.Split(view, "\n"), false
	for i, l := range lines {
		if strings.Contains(ansi.Strip(l), "Anna Petrova @anna · just now") {
			found = true
			if i+2 >= len(lines) || strings.TrimSpace(strings.Trim(ansi.Strip(lines[i+1]), "│ ")) != "" || !strings.ContainsRune(lines[i+2], kitty.Placeholder) {
				t.Errorf("no air between the header and the picture:\n%s\n%s", ansi.Strip(lines[i+1]), ansi.Strip(lines[i+2]))
			}
		}
	}
	if !found {
		t.Errorf("the pasted picture's comment is not on screen:\n%s", ansi.Strip(view))
	}
	d.has("file log.txt") // not a picture: still a link
	d.tick()
	d.tick()
	if len(pics.fetched) != 2 {
		t.Errorf("downloads = %v, want each picture once", pics.fetched)
	}

	// BE-123's agent is working: the loader, not the spinner.
	w := drive(t, NewRun(&f.d, "backend", f.runs["BE-123"].Number, false), 160, 60)
	if !strings.Contains(w.m.View(), "a=a,i=") {
		t.Errorf("the loader doesn't play:\n%s", ansi.Strip(w.m.View()))
	}
	w.hasNot("working…")
}

// TestPicturesOnOneLine: a line of pictures only, with YouTrack's
// {width=…} after any of them, draws each; in text the attributes don't
// show.
func TestPicturesOnOneLine(t *testing.T) {
	var drawn []string
	draw := func(alt, src string, w int) []string {
		drawn = append(drawn, src)
		return []string{"<" + src + ">"}
	}
	got := mdLinesWith("Before\n\n![](a.png){width=70%}![](b.png) ![x](c.png){width=10%}\n\nsee ![](d.png){width=5%} here", 80, draw)
	if strings.Join(drawn, " ") != "a.png b.png c.png" {
		t.Errorf("drawn = %q", drawn)
	}
	text := ansi.Strip(strings.Join(got, "\n"))
	if !strings.Contains(text, "<a.png>\n\n<b.png>\n\n<c.png>") || !strings.Contains(text, "see [image] here") || strings.Contains(text, "{width") {
		t.Errorf("lines = %q", text)
	}
}
