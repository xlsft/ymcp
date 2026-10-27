package vcs

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Repo is a repository on a code host, as found from the origin URL.
type Repo struct {
	Host  string // e.g. github.com, gitlab.example.com, or host:port of an http(s) remote
	Owner string // the user, organisation or (GitLab) group path, e.g. team/sub
	Name  string // the repository name
	Path  string // Owner/Name
	Kind  Kind   // the strategy the host suggests: github-pr, gitlab-mr, or branch when unknown
	API   string // the REST API root, e.g. https://api.github.com; "" when the host is unknown
}

// ParseRemote reads a git remote URL: scp-like (git@host:owner/name.git),
// ssh://, git://, http(s)://. github.com and hosts named like GitHub are
// GitHub (Enterprise at /api/v3), gitlab.com and hosts named like GitLab are
// GitLab (/api/v4); other hosts get Kind branch and no API, which New fills in
// for whatever strategy is picked. Credentials in the URL are dropped.
func ParseRemote(originURL string) (Repo, error) {
	raw := strings.TrimSpace(originURL)
	if raw == "" {
		return Repo{}, fmt.Errorf("vcs: empty remote URL")
	}
	var host, path, scheme string
	if i := strings.Index(raw, "://"); i > 0 {
		u, err := url.Parse(raw)
		if err != nil {
			// url's error repeats the URL, credentials included.
			return Repo{}, fmt.Errorf("vcs: remote %q is not a valid URL", redact(raw))
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http":
			scheme, host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
		case "ssh", "git+ssh", "ssh+git", "git":
			scheme, host = "https", strings.ToLower(u.Hostname()) // the ssh port isn't the web port
		default:
			return Repo{}, fmt.Errorf("vcs: remote %q is not on a code host", redact(raw))
		}
		path = u.Path
	} else {
		// scp-like: [user@]host:path. A colon after a slash is a local path.
		colon := strings.Index(raw, ":")
		if colon <= 0 || strings.Contains(raw[:colon], "/") || strings.HasPrefix(raw[colon+1:], "//") {
			return Repo{}, fmt.Errorf("vcs: remote %q is not on a code host", redact(raw))
		}
		host = raw[:colon]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		scheme, host, path = "https", strings.ToLower(host), raw[colon+1:]
	}
	if host == "" || (len(host) == 1 && scheme == "https") {
		// "c:/repo" on Windows is a drive, not a host.
		return Repo{}, fmt.Errorf("vcs: remote %q is not on a code host", redact(raw))
	}
	host = canonicalHost(host)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	segs := strings.Split(path, "/")
	if len(segs) < 2 || slicesHasEmpty(segs) {
		return Repo{}, fmt.Errorf("vcs: remote %q has no owner/name path", redact(raw))
	}
	r := Repo{
		Host:  host,
		Owner: strings.Join(segs[:len(segs)-1], "/"),
		Name:  segs[len(segs)-1],
		Path:  path,
		Kind:  KindBranch,
	}
	switch family(host) {
	case familyGitHub:
		r.Kind = KindGitHubPR
		if len(segs) != 2 {
			return Repo{}, fmt.Errorf("vcs: remote %q: a GitHub repository path is owner/name", redact(raw))
		}
	case familyGitLab:
		r.Kind = KindGitLabMR
	}
	r.API = apiRoot(familyOfKind(r.Kind), scheme, host)
	return r, nil
}

func slicesHasEmpty(s []string) bool {
	for _, v := range s {
		if v == "" {
			return true
		}
	}
	return false
}

// canonicalHost maps the ssh-over-443 hosts to the web host.
func canonicalHost(h string) string {
	switch h {
	case "ssh.github.com":
		return "github.com"
	case "altssh.gitlab.com":
		return "gitlab.com"
	}
	return h
}

const (
	familyGitHub = "github"
	familyGitLab = "gitlab"
)

// family guesses the kind of host from its name.
func family(host string) string {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	switch {
	case name == "github.com" || strings.Contains(name, "github"):
		return familyGitHub
	case name == "gitlab.com" || strings.Contains(name, "gitlab"):
		return familyGitLab
	}
	return ""
}

func familyOfKind(k Kind) string {
	switch k {
	case KindGitHubPR, KindGitHubIssuePR:
		return familyGitHub
	case KindGitLabMR:
		return familyGitLab
	}
	return ""
}

// apiRoot is the REST root of a host of the family.
func apiRoot(fam, scheme, host string) string {
	if scheme == "" {
		scheme = "https"
	}
	switch fam {
	case familyGitHub:
		if host == "github.com" {
			return "https://api.github.com"
		}
		return scheme + "://" + host + "/api/v3"
	case familyGitLab:
		return scheme + "://" + host + "/api/v4"
	}
	return ""
}

// webRoot is the web address of the host behind an API root.
func webRoot(api string) string {
	if api == "https://api.github.com" {
		return "https://github.com"
	}
	api = strings.TrimSuffix(api, "/")
	for _, s := range []string{"/api/v3", "/api/v4"} {
		if strings.HasSuffix(api, s) {
			return strings.TrimSuffix(api, s)
		}
	}
	return api
}

// redact drops credentials from a URL for error messages.
func redact(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
		return raw[:i+3] + rest[at+1:]
	}
	return raw
}
