package toast

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gotoast "git.sr.ht/~jackmordaunt/go-toast"
	"git.sr.ht/~jackmordaunt/go-toast/tmpl"

	"github.com/xlsft/ymcp/internal/notify"
)

func TestSettings(t *testing.T) {
	s, err := parseSettings(nil)
	if err != nil || !s.sound() {
		t.Errorf("default = %+v, %v", s, err)
	}
	s, err = parseSettings([]byte(`{"enabled":true,"events":["run_done"],"sound":false}`))
	if err != nil || s.sound() || len(s.Events) != 1 {
		t.Errorf("parsed = %+v, %v", s, err)
	}
	if _, err := parseSettings([]byte(`{"volume":3}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := parseSettings([]byte(`{"sound":"loud"}`)); err == nil {
		t.Error("string sound accepted")
	}
	if got := notify.Providers(); !reflect.DeepEqual(got, []string{Name}) {
		t.Errorf("registered providers = %v", got)
	}
}

// renderWindows builds the XML and the PowerShell fallback script the way
// go-toast does, from what toast_windows.go hands it.
func renderWindows(t *testing.T, w winToast) (xmlDoc, script string) {
	t.Helper()
	n := gotoast.Notification{AppID: appID, Title: w.Title, Body: w.Body, Icon: w.Icon,
		ActivationType: gotoast.Foreground, Duration: gotoast.Short, Audio: gotoast.Default}
	if w.Launch != "" {
		n.ActivationType = gotoast.Protocol
		n.ActivationArguments = w.Launch
	}
	if w.Silent {
		n.Audio = gotoast.Silent
	}
	var b bytes.Buffer
	if err := tmpl.XMLTemplate.Execute(&b, n); err != nil {
		t.Fatal(err)
	}
	xmlDoc = b.String()
	b.Reset()
	if err := tmpl.ScriptTemplate.Execute(&b, struct{ AppID, XML string }{appID, xmlDoc}); err != nil {
		t.Fatal(err)
	}
	return xmlDoc, b.String()
}

type parsedToast struct {
	Launch  string `xml:"launch,attr"`
	Type    string `xml:"activationType,attr"`
	Binding struct {
		Image struct {
			Src string `xml:"src,attr"`
		} `xml:"image"`
		Texts []string `xml:"text"`
	} `xml:"visual>binding"`
	Audio struct {
		Silent string `xml:"silent,attr"`
	} `xml:"audio"`
}

func TestWindowsToastEscaping(t *testing.T) {
	e := notify.Event{
		Kind:  notify.KindNeedsInput,
		Title: `BE-1 · "x" ]]> <b>&amp; $env:USERPROFILE ` + "`whoami`",
		Body:  "line1\n\"@\nRemove-Item $HOME\x00\x07 it's > fine]]",
		URL:   `https://yt.example.com/issue/BE-1?a=1&b="2"#focus=Comments-4-1.0-0`,
	}
	icon := `C:\Users\O'Brien & $Co\AppData\Local\ymcp\ymcp.png`
	w := windowsToast(e, icon, false)
	doc, script := renderWindows(t, w)

	var got parsedToast
	if err := xml.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatalf("toast XML does not parse: %v\n%s", err, doc)
	}
	wantTexts := []string{e.Title, "line1\n\"@\nRemove-Item $HOME it's > fine]]"}
	if len(got.Binding.Texts) != 2 || strings.TrimSpace(got.Binding.Texts[0]) != wantTexts[0] || got.Binding.Texts[1] != wantTexts[1] {
		t.Errorf("texts = %q, want %q", got.Binding.Texts, wantTexts)
	}
	if got.Launch != e.URL || got.Type != "protocol" {
		t.Errorf("launch = %q (%s)", got.Launch, got.Type)
	}
	if got.Binding.Image.Src != icon {
		t.Errorf("icon = %q", got.Binding.Image.Src)
	}
	if got.Audio.Silent != "true" {
		t.Error("sound off did not make the toast silent")
	}

	// The PowerShell fallback pastes the XML into an expandable here-string.
	body := script[strings.Index(script, `@"`)+2 : strings.LastIndex(script, `"@`)]
	if strings.ContainsAny(body, "$`") {
		t.Errorf("here-string expands something:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `"@`) {
			t.Errorf("here-string ends early at %q", line)
		}
	}
}

func TestWindowsToastWithoutURL(t *testing.T) {
	for _, u := range []string{"", "javascript:alert(1)", "/relative", "file:///etc/passwd"} {
		w := windowsToast(notify.Event{Kind: notify.KindRunDone, Pipeline: "p", Run: 1, URL: u}, `C:\i.png`, true)
		if w.Launch != "" || w.Silent {
			t.Errorf("URL %q: %+v", u, w)
		}
		doc, _ := renderWindows(t, w)
		var got parsedToast
		if err := xml.Unmarshal([]byte(doc), &got); err != nil || got.Type != "foreground" || strings.TrimSpace(got.Binding.Texts[0]) != "p #1: done" {
			t.Errorf("URL %q: %+v, %v", u, got, err)
		}
	}
}

func TestSessionBusAddress(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	exists := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, x := range paths {
				if x == p {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		env    map[string]string
		exists []string
		want   string
	}{
		{map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "XDG_RUNTIME_DIR": "/x"}, nil, "unix:path=/run/user/1000/bus"},
		{map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, []string{"/run/user/1000/bus"}, "unix:path=/run/user/1000/bus"},
		{map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, nil, ""},
		{map[string]string{"DISPLAY": ":0"}, nil, ""},
	}
	for _, c := range cases {
		got, err := sessionBusAddress(env(c.env), exists(c.exists...))
		if got != c.want || (c.want == "") != errors.Is(err, errNoSessionBus) {
			t.Errorf("%v: %q, %v", c.env, got, err)
		}
	}
}

func TestIconDir(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"XDG_DATA_HOME": "/data", "HOME": "/home/u"}, "/data/ymcp"},
		{map[string]string{"XDG_DATA_HOME": "relative", "HOME": "/home/u"}, "/home/u/.local/share/ymcp"},
		{map[string]string{"HOME": "/home/u"}, "/home/u/.local/share/ymcp"},
	} {
		if got, err := iconDir(env(c.env)); err != nil || filepath.ToSlash(got) != c.want {
			t.Errorf("%v: %q, %v", c.env, got, err)
		}
	}
	if _, err := iconDir(env(nil)); err == nil {
		t.Error("no HOME accepted")
	}
}

func TestFreedesktopNote(t *testing.T) {
	e := notify.Event{Kind: notify.KindNeedsInput, Pipeline: "backend", Run: 17, Issue: "BE-123",
		Body: "Use <b>ISO</b> & UTC?", URL: "https://yt.example.com/issue/BE-123"}
	cases := []struct {
		name  string
		caps  []string
		sound bool
		want  fdNote
	}{
		{"actions", []string{"actions", "body"}, true,
			fdNote{Summary: "BE-123 · backend #17: needs input", Body: "Use <b>ISO</b> & UTC?", Action: true}},
		{"no actions: URL in the text", []string{"body"}, true,
			fdNote{Summary: "BE-123 · backend #17: needs input", Body: "Use <b>ISO</b> & UTC?\nhttps://yt.example.com/issue/BE-123"}},
		{"markup escaped", []string{"actions", "body-markup"}, false,
			fdNote{Summary: "BE-123 · backend #17: needs input", Body: "Use &lt;b&gt;ISO&lt;/b&gt; &amp; UTC?", Action: true, Silent: true}},
	}
	for _, c := range cases {
		if got := freedesktopNote(e, c.caps, c.sound); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	// Nothing to open: no action, nothing added.
	e.URL = ""
	if got := freedesktopNote(e, []string{"actions"}, true); got.Action || got.Body != e.Body {
		t.Errorf("no URL: %+v", got)
	}
	e.Body = ""
	e.URL = "https://x.example.com"
	if got := freedesktopNote(e, nil, true); got.Body != "https://x.example.com" {
		t.Errorf("empty body: %+v", got)
	}
}

func TestInfoPlist(t *testing.T) {
	p := infoPlist("v1.4.2", "abc123")
	var doc struct {
		Keys    []string `xml:"dict>key"`
		Strings []string `xml:"dict>string"`
	}
	if err := xml.Unmarshal(p, &doc); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i, s := range doc.Strings {
		values[doc.Keys[i]] = s
	}
	for k, want := range map[string]string{
		"CFBundleIdentifier":         "dev.xlsft.ymcp",
		"CFBundleExecutable":         "ymcp",
		"CFBundleIconFile":           "ymcp",
		"CFBundleShortVersionString": "1.4.2",
		"YMCPBinarySHA256":           "abc123",
	} {
		if values[k] != want {
			t.Errorf("%s = %q, want %q", k, values[k], want)
		}
	}
	if !bytes.Contains(p, []byte("<key>LSUIElement</key><true/>")) {
		t.Error("not an agent app (LSUIElement)")
	}
	if bytes.Equal(p, infoPlist("v1.4.2", "def456")) {
		t.Error("a new binary hash doesn't change the plist")
	}
	if !bytes.Equal(infoPlist("(devel)", "h"), infoPlist("", "h")) {
		t.Error("dev versions differ")
	}
}

func TestBundleVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.4.2":                             "1.4.2",
		"1.4.2":                              "1.4.2",
		"v0.9.0-0.20261027120000-abcdef1234": "0.9.0",
		"(devel)":                            "0.0.0",
		"":                                   "0.0.0",
		"dev":                                "0.0.0",
	} {
		if got := bundleVersion(in); got != want {
			t.Errorf("bundleVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBundleUpToDate(t *testing.T) {
	app := filepath.Join(t.TempDir(), bundleName)
	plist, icon := infoPlist("v1.0.0", "h1"), []byte("icns")
	write := func(rel string, data []byte) {
		p := filepath.Join(app, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if bundleUpToDate(app, plist, icon) {
		t.Fatal("missing bundle is up to date")
	}
	write("Contents/Info.plist", plist)
	write("Contents/Resources/ymcp.icns", icon)
	write("Contents/MacOS/ymcp", []byte("bin"))
	if bundleUpToDate(app, plist, icon) {
		t.Error("unsigned bundle is up to date")
	}
	write("Contents/_CodeSignature/CodeResources", []byte("sig"))
	if !bundleUpToDate(app, plist, icon) {
		t.Error("complete bundle is not up to date")
	}
	if bundleUpToDate(app, infoPlist("v1.0.0", "h2"), icon) {
		t.Error("bundle of another binary is up to date")
	}
	if bundleUpToDate(app, plist, []byte("new icon")) {
		t.Error("bundle with an old icon is up to date")
	}
}

func TestCheckBundleDir(t *testing.T) {
	home := t.TempDir()
	tmp := t.TempDir()
	link := filepath.Join(home, "tmplink")
	if err := os.Symlink(tmp, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	temps := []string{tmp}
	if err := checkBundleDir(filepath.Join(tmp, "ymcp", bundleName), temps); err == nil {
		t.Error("bundle under a temp dir accepted")
	}
	if err := checkBundleDir(filepath.Join(link, "not-yet", bundleName), temps); err == nil {
		t.Error("bundle under a symlink to a temp dir accepted")
	}
	if err := checkBundleDir(filepath.Join(home, "Library", "Application Support", "ymcp", bundleName), temps); err != nil {
		t.Errorf("normal location refused: %v", err)
	}
}

func TestInBundleAndClickArgs(t *testing.T) {
	for exe, want := range map[string]bool{
		"/Users/u/Library/Application Support/ymcp/ymcp.app/Contents/MacOS/ymcp": true,
		"/usr/local/bin/ymcp":                         false,
		"/Applications/Other.app/Contents/MacOS/ymcp": false,
		"/x/ymcp.app/Contents/MacOS/other":            false,
	} {
		if got := inBundle(exe); got != want {
			t.Errorf("inBundle(%q) = %v", exe, got)
		}
	}
	if !clickArgs(nil) || !clickArgs([]string{"-psn_0_1234"}) || clickArgs([]string{"notify", "post"}) {
		t.Error("clickArgs")
	}
}

func TestAuthStatusProblem(t *testing.T) {
	for _, c := range []struct {
		st   authStatus
		want string // substring of the error; "" for none
	}{
		{authStatus{"authorized", 1}, ""},
		{authStatus{"provisional", 2}, ""},
		{authStatus{"notDetermined", 0}, ""},
		{authStatus{"denied", 0}, "turned off"},
		{authStatus{"authorized", 0}, "alert style is None"},
		{authStatus{"weird", 1}, "unexpected"},
	} {
		err := c.st.problem()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v", c.st, err)
		}
		if err != nil && c.want != "unexpected" && !strings.Contains(err.Error(), settingsHint) {
			t.Errorf("%+v: no hint where to fix it: %v", c.st, err)
		}
	}
}
