// Package update replaces the running ymcp binary with the latest GitHub release.
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Updater talks to the GitHub releases of one repository.
type Updater struct {
	API  string // e.g. https://api.github.com
	Repo string // owner/name
	HTTP *http.Client
	OS   string // runtime.GOOS
	Arch string // runtime.GOARCH
}

// Release is the part of a GitHub release ymcp needs.
type Release struct {
	Tag    string `json:"tag_name"`
	URL    string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r Release) asset(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, true
		}
	}
	return "", false
}

// ArchiveName is the release asset for a platform, as the release pipeline names it.
func ArchiveName(goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("ymcp_%s_%s%s", goos, goarch, ext)
}

// Latest returns the newest published release.
func (u Updater) Latest(ctx context.Context) (Release, error) {
	var r Release
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.API+"/repos/"+u.Repo+"/releases/latest", nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return r, errors.New("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("GitHub: %s", resp.Status)
	}
	return r, json.NewDecoder(resp.Body).Decode(&r)
}

// LatestTag returns the tag of the newest release from the web site's
// releases/latest redirect, which unlike the API has no hourly limit, so
// it suits a check on every start. web is e.g. https://github.com.
func (u Updater) LatestTag(ctx context.Context, web string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, web+"/"+u.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := *u.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/releases/tag/")
	if i < 0 {
		return "", fmt.Errorf("GitHub: %s, no release tag in the redirect", resp.Status)
	}
	tag, err := url.PathUnescape(loc[i+len("/releases/tag/"):])
	if err != nil || tag == "" {
		return "", fmt.Errorf("GitHub: bad release redirect %q", loc)
	}
	return tag, nil
}

// Newer reports whether release tag is newer than the current version.
// Development builds ("dev") are never considered up to date.
func Newer(tag, current string) bool {
	if current == "dev" || current == "" {
		return true
	}
	a, b := parse(tag), parse(current)
	for i := range 3 {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func parse(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "-")
	for i, part := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

// Install downloads the release for this platform, checks its SHA-256
// against checksums.txt and replaces the binary at exe.
func (u Updater) Install(ctx context.Context, r Release, exe string) error {
	name := ArchiveName(u.OS, u.Arch)
	archiveURL, ok := r.asset(name)
	if !ok {
		return fmt.Errorf("release %s has no build for %s/%s", r.Tag, u.OS, u.Arch)
	}
	sumsURL, ok := r.asset("checksums.txt")
	if !ok {
		return fmt.Errorf("release %s has no checksums.txt", r.Tag)
	}
	sums, err := u.get(ctx, sumsURL)
	if err != nil {
		return err
	}
	archive, err := u.get(ctx, archiveURL)
	if err != nil {
		return err
	}
	if err := verify(sums, name, archive); err != nil {
		return err
	}
	bin, err := extract(name, archive, u.OS)
	if err != nil {
		return err
	}
	return replace(exe, bin, u.OS)
}

func (u Updater) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func verify(sums []byte, name string, data []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if fields[0] != got {
				return fmt.Errorf("checksum mismatch for %s", name)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt has no entry for %s", name)
}

func extract(name string, archive []byte, goos string) ([]byte, error) {
	want := "ymcp"
	if goos == "windows" {
		want = "ymcp.exe"
	}
	if strings.HasSuffix(name, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == want {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s not found in %s", want, name)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in %s", want, name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == want {
			return io.ReadAll(tr)
		}
	}
}

// replace swaps the binary at exe for bin. A running executable can't be
// overwritten on Windows, so it is moved aside first.
func replace(exe string, bin []byte, goos string) error {
	mode := os.FileMode(0o755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, mode); err != nil {
		return fmt.Errorf("write %s: %w (try with sudo if ymcp is installed system-wide)", tmp, err)
	}
	if goos == "windows" {
		old := exe + ".old"
		_ = os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
