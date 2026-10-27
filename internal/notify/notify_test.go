package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/pipeline/spec"
)

// fake is a provider that records what it is asked to do.
type fake struct {
	name     string
	onByDflt bool
	settings struct {
		Common
		Level *int `json:"level,omitempty"`
	}
	sendErr  error
	checkErr error
	delay    time.Duration
	panics   bool

	mu     sync.Mutex
	sent   []Event
	closed int
}

func (f *fake) Name() string { return f.name }
func (f *fake) Configure(raw json.RawMessage) error {
	if err := DecodeSettings(raw, &f.settings); err != nil {
		return err
	}
	if f.settings.Level != nil && *f.settings.Level < 0 {
		return errors.New("level must not be negative")
	}
	return nil
}
func (f *fake) Check(context.Context) error { return f.checkErr }
func (f *fake) Send(ctx context.Context, e Event) error {
	if f.panics {
		panic("boom")
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.sent = append(f.sent, e)
	f.mu.Unlock()
	return f.sendErr
}
func (f *fake) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}
func (f *fake) EnabledByDefault() bool { return f.onByDflt }

func (f *fake) events() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// withProviders swaps the registry for the test; the returned map holds
// the provider instance New created last for each name.
func withProviders(t *testing.T, protos ...*fake) map[string]*fake {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = map[string]func() Provider{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
	var mu sync.Mutex
	made := map[string]*fake{}
	for _, p := range protos {
		Register(p.name, func() Provider {
			f := &fake{name: p.name, onByDflt: p.onByDflt, sendErr: p.sendErr, checkErr: p.checkErr, delay: p.delay, panics: p.panics}
			mu.Lock()
			made[p.name] = f
			mu.Unlock()
			return f
		})
	}
	return made
}

func TestRegistry(t *testing.T) {
	withProviders(t, &fake{name: "b"}, &fake{name: "a"})
	if got := Providers(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Providers = %v", got)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("duplicate Register did not panic")
			}
		}()
		Register("a", func() Provider { return &fake{name: "a"} })
	}()
}

func TestSettingsRoundTrip(t *testing.T) {
	t.Setenv("YMCP_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	withProviders(t, &fake{name: "toast", onByDflt: true}, &fake{name: "ntfy"})

	s, err := Load()
	if err != nil || len(s.Providers) != 0 {
		t.Fatalf("empty Load = %+v, %v", s, err)
	}
	for _, kv := range [][2]string{
		{"toast.enabled", "false"},
		{"toast.events", "needs_input, review_ready,needs_input"},
		{"toast.level", "3"},
		{"ntfy.enabled", "true"},
	} {
		if err := s.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("Set(%s) = %v", kv[0], err)
		}
	}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Providers["toast"]) != `{"enabled":false,"events":["needs_input","review_ready"],"level":3}` {
		t.Errorf("toast = %s", got.Providers["toast"])
	}
	states, err := got.State()
	if err != nil {
		t.Fatal(err)
	}
	want := []ProviderState{
		{Name: "ntfy", Enabled: true, Events: DefaultEvents, Settings: got.Providers["ntfy"]},
		{Name: "toast", Enabled: false, Events: []Kind{KindNeedsInput, KindReviewReady}, Settings: got.Providers["toast"]},
	}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("State = %+v\nwant %+v", states, want)
	}

	// An empty value brings the default back; no fields left drops the section.
	for _, k := range []string{"toast.enabled", "toast.events", "toast.level"} {
		if err := got.Set(k, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := got.Providers["toast"]; ok {
		t.Errorf("toast section kept: %s", got.Providers["toast"])
	}
	states, _ = got.State()
	if !states[1].Enabled {
		t.Error("toast not back on by default")
	}
	got.Set("ntfy.enabled", "")
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(); len(again.Providers) != 0 {
		t.Errorf("after clearing = %+v", again)
	}
}

func TestSetRejects(t *testing.T) {
	withProviders(t, &fake{name: "toast"})
	var s Settings
	for key, value := range map[string]string{
		"toast":          "x",
		"nope.enabled":   "true",
		"toast.enabled":  "maybe",
		"toast.events":   "needs_input,party",
		"toast.typo":     "1",
		"toast.level":    "-1",
		".enabled":       "true",
		"toast.events  ": ",",
	} {
		if err := s.Set(key, value); err == nil {
			t.Errorf("Set(%q, %q) succeeded", key, value)
		}
	}
	if len(s.Providers) != 0 {
		t.Errorf("rejected settings were stored: %v", s.Providers)
	}
}

func TestFilter(t *testing.T) {
	cases := []struct {
		name     string
		settings string
		filter   *Filter
		want     map[string][]Kind // enabled providers and their events
	}{
		{"defaults", `{}`, nil, map[string][]Kind{"toast": DefaultEvents}},
		{"opt-in provider", `{"providers":{"ntfy":{"enabled":true,"events":["run_done"]}}}`, nil,
			map[string][]Kind{"toast": DefaultEvents, "ntfy": {KindRunDone}}},
		{"turned off", `{"providers":{"toast":{"enabled":false}}}`, nil, map[string][]Kind{}},
		{"pipeline narrows providers", `{"providers":{"ntfy":{"enabled":true}}}`, &Filter{Providers: []string{"ntfy"}},
			map[string][]Kind{"ntfy": DefaultEvents}},
		{"pipeline sets events", `{}`, &Filter{Events: []Kind{KindReviewReady}},
			map[string][]Kind{"toast": {KindReviewReady}}},
		{"pipeline can't turn on a disabled provider", `{"providers":{"toast":{"enabled":false}}}`, &Filter{Providers: []string{"toast"}},
			map[string][]Kind{}},
		{"unknown stored provider ignored", `{"providers":{"telegram":{"enabled":true}}}`, nil, map[string][]Kind{"toast": DefaultEvents}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withProviders(t, &fake{name: "toast", onByDflt: true}, &fake{name: "ntfy"})
			var s Settings
			if err := json.Unmarshal([]byte(c.settings), &s); err != nil {
				t.Fatal(err)
			}
			n, err := New(s, c.filter, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string][]Kind{}
			for _, sk := range n.sinks {
				got[sk.name] = sk.events
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("enabled = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNewRejects(t *testing.T) {
	withProviders(t, &fake{name: "toast", onByDflt: true})
	for name, c := range map[string]struct {
		settings string
		filter   *Filter
	}{
		"bad provider settings": {`{"providers":{"toast":{"level":-1}}}`, nil},
		"unknown field":         {`{"providers":{"toast":{"volume":1}}}`, nil},
		"bad stored event":      {`{"providers":{"toast":{"events":["x"]}}}`, nil},
		"filter provider":       {`{}`, &Filter{Providers: []string{"nope"}}},
		"filter event":          {`{}`, &Filter{Events: []Kind{"nope"}}},
	} {
		var s Settings
		json.Unmarshal([]byte(c.settings), &s)
		if _, err := New(s, c.filter, nil); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
}

func TestFilterOf(t *testing.T) {
	if FilterOf(nil, nil) != nil {
		t.Error("empty lists gave a filter")
	}
	f := FilterOf([]string{"toast"}, []string{"run_done"})
	if !reflect.DeepEqual(f, &Filter{Providers: []string{"toast"}, Events: []Kind{KindRunDone}}) {
		t.Errorf("FilterOf = %+v", f)
	}
}

func TestNotifyIsAsyncAndFiltered(t *testing.T) {
	made := withProviders(t, &fake{name: "toast", onByDflt: true, delay: 200 * time.Millisecond})
	n, err := New(Settings{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	n.Notify(Event{Kind: KindNeedsInput, Pipeline: "backend", Run: 17, Issue: "BE-123"})
	n.Notify(Event{Kind: KindRunDone, Pipeline: "backend", Run: 17}) // not in the default events
	n.Notify(Event{Kind: KindTest})                                  // always delivered
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Notify blocked for %v", d)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	f := made["toast"]
	var kinds []Kind
	for _, e := range f.events() {
		kinds = append(kinds, e.Kind)
	}
	slices.Sort(kinds)
	if !reflect.DeepEqual(kinds, []Kind{KindNeedsInput, KindTest}) {
		t.Errorf("sent %v", kinds)
	}
	if f.closed != 1 {
		t.Errorf("closed %d times", f.closed)
	}
	n.Notify(Event{Kind: KindNeedsInput})
	n.Close()
	if len(f.events()) != 2 || f.closed != 1 {
		t.Error("Notify or Close did something after Close")
	}
}

func TestNotifyErrorsAreLoggedAndReported(t *testing.T) {
	withProviders(t,
		&fake{name: "toast", onByDflt: true, sendErr: errors.New("denied")},
		&fake{name: "boom", onByDflt: true, panics: true},
	)
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &buf, mu: &mu}, nil))
	n, err := New(Settings{}, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	var reported []string
	var rmu sync.Mutex
	n.OnError(func(e Event, provider string, err error) {
		rmu.Lock()
		reported = append(reported, provider+": "+err.Error())
		rmu.Unlock()
	})
	n.Notify(Event{Kind: KindRunFailed, Pipeline: "backend", Run: 3})
	n.Close()
	slices.Sort(reported)
	if len(reported) != 2 || !strings.Contains(reported[0], "boom: provider panicked") || reported[1] != "toast: denied" {
		t.Errorf("reported %q", reported)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "notification failed") || !strings.Contains(buf.String(), "err=denied") {
		t.Errorf("log = %s", buf.String())
	}
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestTimeouts(t *testing.T) {
	withProviders(t, &fake{name: "slow", onByDflt: true, delay: 2 * time.Second})
	n, err := New(Settings{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.timeout = 50 * time.Millisecond
	start := time.Now()
	res := n.Deliver(context.Background(), Event{Kind: KindNeedsInput})
	if d := time.Since(start); d > time.Second {
		t.Errorf("Deliver waited %v", d)
	}
	if err := res["slow"]; err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Deliver = %v", res)
	}
	var reported error
	done := make(chan struct{})
	n.OnError(func(_ Event, _ string, err error) { reported = err; close(done) })
	n.Notify(Event{Kind: KindNeedsInput})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("async send did not time out")
	}
	if !errors.Is(reported, context.DeadlineExceeded) {
		t.Errorf("reported %v", reported)
	}
}

func TestDeliverAndCheck(t *testing.T) {
	withProviders(t,
		&fake{name: "toast", onByDflt: true},
		&fake{name: "ntfy", checkErr: errors.New("no server")},
	)
	s := Settings{Providers: map[string]json.RawMessage{"ntfy": json.RawMessage(`{"enabled":true,"events":["run_done"]}`)}}
	n, err := New(s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if got := n.Enabled(); !reflect.DeepEqual(got, []string{"ntfy", "toast"}) {
		t.Errorf("Enabled = %v", got)
	}
	res := n.Deliver(context.Background(), Event{Kind: KindNeedsInput})
	if keys := slices.Sorted(maps.Keys(res)); !reflect.DeepEqual(keys, []string{"toast"}) || res["toast"] != nil {
		t.Errorf("Deliver needs_input = %v", res)
	}
	res = n.Deliver(context.Background(), Event{Kind: KindTest})
	if len(res) != 2 {
		t.Errorf("Deliver test = %v", res)
	}
	chk := n.Check(context.Background())
	if len(chk) != 2 || chk["toast"] != nil || chk["ntfy"] == nil {
		t.Errorf("Check = %v", chk)
	}
}

func TestHeading(t *testing.T) {
	for _, c := range []struct {
		e    Event
		want string
	}{
		{Event{Kind: KindNeedsInput, Pipeline: "backend", Run: 17, Issue: "BE-123"}, "BE-123 · backend #17: needs input"},
		{Event{Kind: KindRunFailed, Pipeline: "backend", Run: 2}, "backend #2: failed"},
		{Event{Kind: KindRunnerStopped, Pipeline: "backend"}, "backend: runner stopped"},
		{Event{Kind: KindTest}, "ymcp: test"},
		{Event{Kind: "custom_thing"}, "ymcp: custom thing"},
		{Event{Kind: KindRunDone, Title: "Custom"}, "Custom"},
	} {
		if got := c.e.Heading(); got != c.want {
			t.Errorf("Heading(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestParseKind(t *testing.T) {
	if k, err := ParseKind(" review_ready "); err != nil || k != KindReviewReady {
		t.Errorf("ParseKind = %q, %v", k, err)
	}
	if _, err := ParseKind("test"); err == nil {
		t.Error("test accepted as a settings event")
	}
}

func TestKindsMatchSpec(t *testing.T) {
	var kinds []string
	for _, k := range Kinds {
		kinds = append(kinds, string(k))
	}
	if !reflect.DeepEqual(kinds, spec.NotificationEvents) {
		t.Errorf("notify.Kinds %v != spec.NotificationEvents %v", kinds, spec.NotificationEvents)
	}
}
