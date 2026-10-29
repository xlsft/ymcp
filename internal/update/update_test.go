package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func fakeGitHub(t *testing.T, archive []byte, sum string) *Updater {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/xlsft/ymcp/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v1.2.0","assets":[
			{"name":"ymcp_linux_amd64.tar.gz","browser_download_url":"%[1]s/dl/archive"},
			{"name":"checksums.txt","browser_download_url":"%[1]s/dl/sums"}]}`, srv.URL)
	})
	mux.HandleFunc("/dl/archive", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "deadbeef  ymcp_darwin_arm64.tar.gz\n%s  ymcp_linux_amd64.tar.gz\n", sum)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Updater{API: srv.URL, Repo: "xlsft/ymcp", HTTP: srv.Client(), OS: "linux", Arch: "amd64"}
}

func TestInstallReplacesBinary(t *testing.T) {
	archive := tarGz(t, "ymcp", []byte("new binary"))
	sum := sha256.Sum256(archive)
	u := fakeGitHub(t, archive, hex.EncodeToString(sum[:]))

	r, err := u.Latest(context.Background())
	if err != nil || r.Tag != "v1.2.0" {
		t.Fatalf("Latest = %+v, %v", r, err)
	}
	exe := filepath.Join(t.TempDir(), "ymcp")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	if err := u.Install(context.Background(), r, exe); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new binary" {
		t.Errorf("binary = %q", data)
	}
	if info, _ := os.Stat(exe); info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	u := fakeGitHub(t, tarGz(t, "ymcp", []byte("evil")), "00")
	r, _ := u.Latest(context.Background())
	exe := filepath.Join(t.TempDir(), "ymcp")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	if err := u.Install(context.Background(), r, exe); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if data, _ := os.ReadFile(exe); string(data) != "old binary" {
		t.Errorf("binary changed to %q", data)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, cur string
		want     bool
	}{
		{"v1.2.0", "1.1.9", true},
		{"v1.2.0", "1.2.0", false},
		{"v1.10.0", "1.9.3", true},
		{"v1.2.0", "v1.3.0", false},
		{"v0.1.0", "dev", true},
	} {
		if got := Newer(c.tag, c.cur); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.tag, c.cur, got)
		}
	}
}

func TestExtractZipAndReplaceOnWindows(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ymcp_windows_amd64/ymcp.exe")
	w.Write([]byte("exe"))
	zw.Close()
	bin, err := extract("ymcp_windows_amd64.zip", buf.Bytes(), "windows")
	if err != nil || string(bin) != "exe" {
		t.Fatalf("extract = %q, %v", bin, err)
	}
	if _, err := extract("x.zip", buf.Bytes(), "linux"); err == nil {
		t.Error("missing ymcp in zip not reported")
	}
	if ArchiveName("windows", "arm64") != "ymcp_windows_arm64.zip" || ArchiveName("linux", "amd64") != "ymcp_linux_amd64.tar.gz" {
		t.Error("ArchiveName")
	}

	exe := filepath.Join(t.TempDir(), "ymcp.exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := replace(exe, []byte("new"), "windows"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(exe); string(data) != "new" {
		t.Errorf("exe = %q", data)
	}
	if data, _ := os.ReadFile(exe + ".old"); string(data) != "old" {
		t.Errorf("old exe = %q", data)
	}
}

func TestLatestTagFollowsNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xlsft/ymcp/releases/latest":
			http.Redirect(w, r, "/xlsft/ymcp/releases/tag/v0.5.0", http.StatusFound)
		case "/none/ymcp/releases/latest":
			http.Redirect(w, r, "/none/ymcp/releases", http.StatusFound)
		default:
			t.Errorf("followed the redirect to %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	u := Updater{Repo: "xlsft/ymcp", HTTP: srv.Client()}
	if tag, err := u.LatestTag(context.Background(), srv.URL); err != nil || tag != "v0.5.0" {
		t.Errorf("LatestTag = %q, %v", tag, err)
	}
	u.Repo = "none/ymcp"
	if _, err := u.LatestTag(context.Background(), srv.URL); err == nil {
		t.Error("no tag in the redirect, but no error")
	}
}

func TestChangesBetweenVersions(t *testing.T) {
	notes := func(tag string) string {
		return "## What's new\n\n### Added\n\n- " + tag + " adds `x`,\n  continued.\n\n### Fixed\n\n- " + tag +
			" fixes y.\n\n## Changelog\n### Features\n* abc: feat: z (@xlsft)\n\n## Install\n- not an entry\n"
	}
	// Two pages, newest first as GitHub lists them.
	pages := [][]string{
		{
			`{"tag_name":"v0.5.0","body":"draft","draft":true}`,
			`{"tag_name":"v0.4.1-rc.1","body":"pre","prerelease":true}`,
			`{"tag_name":"v0.4.0","body":` + quote(notes("v0.4.0")) + `}`,
			`{"tag_name":"v0.3.0","body":"## Changelog\n* no What's new"}`,
		},
		{
			`{"tag_name":"v0.2.1","body":` + quote(strings.ReplaceAll(notes("v0.2.1"), "\n", "\r\n")) + `}`,
			`{"tag_name":"v0.2.0","body":` + quote(notes("v0.2.0")) + `}`,
			`{"tag_name":"v0.1.0","body":` + quote(notes("v0.1.0")) + `}`,
		},
	}
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery)
		page := map[string]int{"1": 0, "2": 1}[r.URL.Query().Get("page")]
		list := pages[page]
		if page == 0 {
			// A full page: pad it to 100 with releases newer than latest.
			for len(list) < 100 {
				list = append([]string{`{"tag_name":"v9.0.0","body":` + quote(notes("v9.0.0")) + `}`}, list...)
			}
		}
		fmt.Fprint(w, "["+strings.Join(list, ",")+"]")
	}))
	defer srv.Close()
	u := Updater{API: srv.URL, Repo: "xlsft/ymcp", HTTP: srv.Client()}

	got, err := u.Changes(context.Background(), "0.2.0", "v0.4.1")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, n := range got {
		tags = append(tags, n.Tag)
	}
	if strings.Join(tags, " ") != "v0.4.0 v0.2.1" {
		t.Fatalf("tags = %v, want v0.4.0 v0.2.1 (asked %v)", tags, asked)
	}
	want := []Section{
		{Title: "Added", Entries: []string{"v0.2.1 adds `x`, continued."}},
		{Title: "Fixed", Entries: []string{"v0.2.1 fixes y."}},
	}
	if fmt.Sprint(got[1].Sections) != fmt.Sprint(want) {
		t.Errorf("v0.2.1 sections = %q, want %q", got[1].Sections, want)
	}
	if len(asked) != 2 || !strings.Contains(asked[0], "per_page=100") {
		t.Errorf("asked %v, want two pages of 100", asked)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
