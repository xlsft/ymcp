package vcs

import "testing"

// FuzzParseRemote: no origin URL makes ParseRemote panic, and one it
// accepts names a host.
func FuzzParseRemote(f *testing.F) {
	for _, s := range []string{"git@github.com:o/r.git", "https://gitlab.example.ru/team/sub/proj.git", "ssh://git@host:2222/a/b", "/local/path", "", "git@:x", "https://[::1]/a/b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, origin string) {
		repo, err := ParseRemote(origin)
		if err == nil && repo.Host == "" {
			t.Errorf("ParseRemote(%q) = %+v with no host", origin, repo)
		}
	})
}
