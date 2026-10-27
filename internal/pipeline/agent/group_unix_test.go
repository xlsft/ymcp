//go:build !windows

package agent

import (
	"context"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/agent/agenttest"
)

// groups collects what StartOptions.OnGroup is told.
type groups struct {
	mu   sync.Mutex
	list []int
}

func (g *groups) add(pg int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.list = append(g.list, pg)
}

func (g *groups) get() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.list)
}

// TestOnGroupIsTheAgentsGroup: the group OnGroup is told is the one the
// agent and its children run in, here and on a host.
func TestOnGroupIsTheAgentsGroup(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		remote bool
	}{{"claude", false}, {"acp", false}, {"claude", true}, {"acp", true}} {
		name := tc.kind
		if tc.remote {
			name += " on a host"
		}
		t.Run(name, func(t *testing.T) {
			var g groups
			script := agenttest.Script{Turns: [][]agenttest.Step{{{Spawn: true}, {Text: "working"}, {Sleep: time.Minute}}}}
			opts := StartOptions{OnGroup: g.add}
			var s *recorder
			var state string
			if tc.remote {
				s, state, _ = remoteFake(t, tc.kind, script, opts)
			} else {
				s, state = fake(t, tc.kind, script, opts)
			}
			go s.Prompt(context.Background(), "work")
			child := waitChild(t, state)
			pg, err := syscall.Getpgid(child)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for len(g.get()) == 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if got := g.get(); !slices.Equal(got, []int{pg}) {
				t.Errorf("OnGroup was told %v, want [%d]", got, pg)
			}
			s.Close()
			gone(t, child)
		})
	}
}
