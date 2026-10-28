package views

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/store"
)

func TestLogEntriesJoinStreamedText(t *testing.T) {
	at := time.Now()
	ev := func(kind, data string) store.Event {
		return store.Event{At: at, Kind: kind, Data: json.RawMessage(data)}
	}
	got := logEntries([]store.Event{
		ev("agent.thought", `{"text":"Where is "}`),
		ev("agent.thought", `{"text":"the handler?"}`),
		ev("agent.text", `{"text":"Part one, "}`),
		ev("agent.text", `{"text":"part two."}`),
		ev("agent.tool_call", `{"tool":{"title":"Read","status":"in_progress"}}`),
		ev("agent.text", `{"text":"Done."}`),
	})
	var texts []string
	for _, e := range got {
		texts = append(texts, e.kind+": "+e.text)
	}
	want := []string{"agent.thought: Where is the handler?", "agent.text: Part one, part two.", "agent.tool_call: Read", "agent.text: Done."}
	if len(texts) != len(want) {
		t.Fatalf("entries = %q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, texts[i], want[i])
		}
	}
}
