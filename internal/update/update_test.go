package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
