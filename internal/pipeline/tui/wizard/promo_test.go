package wizard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlsft/ymcp/internal/pipeline/agent"
	"github.com/xlsft/ymcp/internal/pipeline/spec"
	"github.com/xlsft/ymcp/internal/pipeline/workspace"
)

// promoAgents finds every agent, as on a machine that has them all.
type promoAgents struct{}

func (promoAgents) Check(_ context.Context, a spec.Agent) (agent.Info, error) {
	switch a.Provider {
	case "claude":
		return agent.Info{Path: "/usr/local/bin/claude", Version: "2.1.0 (Claude Code)"}, nil
	case "kimi":
		return agent.Info{Path: "/usr/local/bin/kimi", Version: "1.4.0"}, nil
	case "opencode":
		return agent.Info{Path: "/opt/homebrew/bin/opencode", Version: "0.15.3"}, nil
	}
	return agent.Info{Path: "/usr/local/bin/gemini", Version: "0.9.0"}, nil
}

// promoRepo is every directory a clean repository with a GitHub origin.
type promoRepo struct{}

func (promoRepo) Inspect(_ context.Context, host, dir string) ([]workspace.Check, error) {
	return []workspace.Check{
		{Name: workspace.CheckRepo, OK: true, Detail: dir},
		{Name: workspace.CheckOrigin, OK: true, Detail: "git@github.com:acme/backend.git"},
		{Name: workspace.CheckDefaultBranch, OK: true, Detail: "main"},
		{Name: workspace.CheckUserEmail, OK: true, Detail: "jane@acme.dev"},
		{Name: workspace.CheckPush, OK: true, Detail: "allowed"},
	}, nil
}

func (promoRepo) Dirs(ctx context.Context, _, dir string) ([]workspace.Dir, error) {
	return workspace.Dirs(ctx, workspace.Local(), dir)
}

type promoDetector struct{}

func (promoDetector) Detect(context.Context, string, string) (Detection, error) {
	return Detection{Kind: "github-pr", Repo: "github.com/acme/backend", Auth: "token from `gh auth token`", AuthOK: true}, nil
}

// TestPromo writes the wizard frames of the promo video to $YMCP_PROMO/wizard,
// with wizard.txt listing "seconds file": the repository on an ssh host,
// typed, then every agent in turn.
func TestPromo(t *testing.T) {
	dir := os.Getenv("YMCP_PROMO")
	if dir == "" {
		t.Skip("YMCP_PROMO not set")
	}
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(0)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	out := filepath.Join(dir, "wizard")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, "code", "backend", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakes()
	f.deps.Agents, f.deps.Workspace, f.deps.Strategy = promoAgents{}, promoRepo{}, promoDetector{}
	d := start(t, f.deps, nil)
	d.send(tea.WindowSizeMsg{Width: 120, Height: 34})

	var list []string
	shoot := func(sec float64) {
		name := fmt.Sprintf("wizard-%04d.ans", len(list))
		if err := os.WriteFile(filepath.Join(out, name), []byte(d.view()), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, fmt.Sprintf("%.4f %s", sec, name))
	}

	// Basics and Board, as a person fills them in.
	d.typ("backend")
	d.keys("enter", "down", "tab")
	d.typ("project: YPC #Unr")
	d.keys("down", "enter")
	d.keys("ctrl+n")
	d.keys("ctrl+n")
	if d.step() != "Repository" {
		t.Fatalf("at %s", d.step())
	}
	d.typ("~/code/backend")
	d.keys("esc")
	d.focus("advanced")
	d.keys("enter")
	d.focus("host")

	// The ssh host, typed.
	sec := 0.0
	shoot(sec)
	for _, r := range "build-box" {
		d.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		sec += 0.09
		shoot(sec)
	}
	d.keys("ctrl+n")
	if d.step() != "Agent" {
		t.Fatalf("at %s:\n%s", d.step(), d.view())
	}
	// Every agent, one an eighth note (0.254 s).
	sec = 1.0
	shoot(sec)
	for range 3 {
		d.keys("right")
		sec += 0.2542
		shoot(sec)
	}
	if err := os.WriteFile(filepath.Join(out, "wizard.txt"), []byte(strings.Join(list, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
