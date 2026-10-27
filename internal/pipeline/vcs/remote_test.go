package vcs

import (
	"strings"
	"testing"
)

func TestParseRemote(t *testing.T) {
	tests := []struct {
		in                      string
		host, owner, name, path string
		kind                    Kind
		api                     string
	}{
		{"git@github.com:xlsft/ymcp.git", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"https://github.com/xlsft/ymcp", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"https://github.com/xlsft/ymcp.git/", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"https://x-access-token:secret@github.com/xlsft/ymcp.git", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"ssh://git@github.com/xlsft/ymcp.git", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"ssh://git@ssh.github.com:443/xlsft/ymcp.git", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"github.com:xlsft/ymcp", "github.com", "xlsft", "ymcp", "xlsft/ymcp", KindGitHubPR, "https://api.github.com"},
		{"git@github.example.com:team/app.git", "github.example.com", "team", "app", "team/app", KindGitHubPR, "https://github.example.com/api/v3"},
		{"git@gitlab.com:group/project.git", "gitlab.com", "group", "project", "group/project", KindGitLabMR, "https://gitlab.com/api/v4"},
		{"git@gitlab.example.ru:team/sub/deep/proj.git", "gitlab.example.ru", "team/sub/deep", "proj", "team/sub/deep/proj", KindGitLabMR, "https://gitlab.example.ru/api/v4"},
		{"ssh://git@gitlab.example.ru:2222/team/sub/proj.git", "gitlab.example.ru", "team/sub", "proj", "team/sub/proj", KindGitLabMR, "https://gitlab.example.ru/api/v4"},
		{"https://gitlab.example.ru:8443/team/sub/proj.git", "gitlab.example.ru:8443", "team/sub", "proj", "team/sub/proj", KindGitLabMR, "https://gitlab.example.ru:8443/api/v4"},
		{"http://GitLab.Local/team/proj", "gitlab.local", "team", "proj", "team/proj", KindGitLabMR, "http://gitlab.local/api/v4"},
		{"git@code.example.com:team/sub/proj.git", "code.example.com", "team/sub", "proj", "team/sub/proj", KindBranch, ""},
	}
	for _, tt := range tests {
		r, err := ParseRemote(tt.in)
		if err != nil {
			t.Errorf("ParseRemote(%q): %v", tt.in, err)
			continue
		}
		want := Repo{Host: tt.host, Owner: tt.owner, Name: tt.name, Path: tt.path, Kind: tt.kind, API: tt.api}
		if r != want {
			t.Errorf("ParseRemote(%q) = %+v, want %+v", tt.in, r, want)
		}
	}
}

func TestParseRemoteErrors(t *testing.T) {
	for _, in := range []string{
		"", "/srv/git/repo.git", "./repo", "file:///srv/repo.git", "C:/repos/x", `C:\repos\x`,
		"git@github.com:onlyname.git", "git@github.com:a/b/c.git", "https://github.com/",
		"https://user:hunter2@code.example.com/x",
	} {
		_, err := ParseRemote(in)
		if err == nil {
			t.Errorf("ParseRemote(%q): want an error", in)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("ParseRemote(%q) error leaks the password: %v", in, err)
		}
	}
}

func TestRedact(t *testing.T) {
	if got := redact("https://u:p@host/x?y@z"); got != "https://host/x?y@z" {
		t.Errorf("redact = %q", got)
	}
	if got := redact("git@host:x"); got != "git@host:x" {
		t.Errorf("redact = %q", got)
	}
}
