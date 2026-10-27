package vcs

import (
	"os"
	"testing"
)

// TestLiveGitHub reads github.com/xlsft/ymcp with the gh login. Read-only:
// it never writes anything. Run with YMCP_PIPELINE_LIVE=1.
func TestLiveGitHub(t *testing.T) {
	if os.Getenv("YMCP_PIPELINE_LIVE") != "1" {
		t.Skip("set YMCP_PIPELINE_LIVE=1 to read github.com/xlsft/ymcp")
	}
	ctx := ctxT(t)
	repo, err := ParseRemote("git@github.com:xlsft/ymcp.git")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := TokenFor(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(KindGitHubPR, repo, auth)
	if err != nil {
		t.Fatal(err)
	}
	me, err := s.Me(ctx)
	if err != nil || me == "" {
		t.Fatalf("Me = %q, %v", me, err)
	}
	ch, err := s.Change(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if ch.State != StateMerged || !ch.Merged || ch.URL != "https://github.com/xlsft/ymcp/pull/3" {
		t.Errorf("PR #3 = %+v", ch)
	}
	_, cur, err := s.Comments(ctx, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Twice more: the second poll settles the since URLs, the third is all 304.
	if _, cur, err = s.Comments(ctx, 3, cur); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Comments(ctx, 3, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an unchanged closed PR returned %d comments", len(got))
	}
	t.Logf("me=%s pr=%+v cursor keys=%d", me, ch, len(cur))
}
