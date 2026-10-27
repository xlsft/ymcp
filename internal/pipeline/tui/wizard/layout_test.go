package wizard

import (
	"testing"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// The wizard's shortcuts work on a Russian layout, where each key types a
// Cyrillic letter; text typed into its fields stays Cyrillic.

func TestCancelRussianLayout(t *testing.T) {
	d := start(t, newFakes().deps, nil)
	// The name field gets the letters as typed, т (n) and н (y) included.
	d.typ("тест-н")
	if got := d.m.sh.p.Name; got != "тест-н" {
		t.Fatalf("name = %q", got)
	}
	if len(d.out) != 0 {
		t.Fatalf("typing in the name did %v", d.out)
	}
	d.keys("esc")
	d.must("Discard your changes?")
	d.typ("т") // n
	d.mustNot("Discard your changes?")
	if len(d.out) != 0 {
		t.Fatal("cancelled on т")
	}
	d.keys("esc")
	d.typ("Н") // Y
	if len(d.out) != 1 || d.out[0] != (Cancelled{}) {
		t.Fatalf("got %v", d.out)
	}
}

func TestAgentStepRussianLayout(t *testing.T) {
	f := newFakes()
	d := start(t, f.deps, nil)
	d.goStep(3)
	d.focus("prompts")
	d.send(editedMsg{which: 0, text: "Mine"})
	d.must("based on fix, modified: start")
	d.typ("к") // r: reset to the preset
	d.must("based on fix")
	d.mustNot("modified")

	// ы (s) opens "save as preset"; its name box takes Cyrillic as typed.
	d.typ("ы")
	d.must("Save these prompts as a preset")
	st := d.m.steps[d.m.cur].(*agentStep)
	d.typ("мой")
	if got := st.save.name.Value(); got != "мой" {
		t.Errorf("preset name = %q", got)
	}
	d.keys("esc")
	d.mustNot("Save these prompts as a preset")

	// The service instruction toggle: ч (x) flips it.
	d.focus("advanced")
	d.keys("enter")
	d.focus("service")
	was := d.m.sh.p.ServiceEnabled()
	d.typ("ч")
	if d.m.sh.p.ServiceEnabled() == was {
		t.Error("ч did not flip the service instruction")
	}

	// In a text field the same letters are typed.
	d.focus("language")
	d.typ("ыч")
	d.mustNot("Save these prompts as a preset")
	if d.m.sh.p.Language != "Englishыч" {
		t.Errorf("language = %q", d.m.sh.p.Language)
	}
}

func TestRepoStepRussianLayout(t *testing.T) {
	f := newFakes()
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-work", Source: spec.Source{Query: "q"},
		Workspace: spec.Workspace{Dir: "/src/repo"}, Strategy: spec.Strategy{Kind: "gitlab-mr"}}
	d := start(t, f.deps, &existing)
	d.goStep(2)
	d.focus("strategy")
	d.must("u use github-pr")
	d.typ("г") // u
	if got := d.m.sh.p.Strategy.Kind; got != "github-pr" {
		t.Fatalf("strategy = %q", got)
	}
	d.focus("draft")
	d.typ("ч") // x
	if !d.m.sh.p.Strategy.Draft {
		t.Error("ч did not tick draft")
	}
}

func TestReviewStepRussianLayout(t *testing.T) {
	f := newFakes()
	existing := spec.Pipeline{Name: "x", Profile: "youtrack-work", Source: spec.Source{Query: "project: YPC #Unresolved"},
		Workspace: spec.Workspace{Dir: "/src/repo"}}
	d := start(t, f.deps, &existing)
	d.goStep(5)
	before := len(f.checker.got)
	d.typ("с") // c: check again
	if len(f.checker.got) != before+1 {
		t.Errorf("с ran %d checks, want 1", len(f.checker.got)-before)
	}
}
