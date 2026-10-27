package toast

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/xlsft/ymcp/internal/notify"
)

// TestMain lets the test binary stand in for ymcp inside the macOS wrapper
// bundle in the live test: the bundle runs its copy as `… notify post …`,
// and macOS relaunches it with no arguments for a click.
func TestMain(m *testing.M) {
	if ClickLaunch() {
		HandleClick()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "notify" {
		if err := HelperMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

// TestLive sends one real notification on this machine. On macOS it builds
// the real wrapper bundle in ~/Library/Application Support/ymcp around this
// test binary (the next ymcp run replaces it).
//
//	YMCP_NOTIFY_LIVE=1 go test -run TestLive -v ./internal/notify/toast/
func TestLive(t *testing.T) {
	if os.Getenv("YMCP_NOTIFY_LIVE") != "1" {
		t.Skip("set YMCP_NOTIFY_LIVE=1 to send a real notification")
	}
	p := newProvider()
	if err := p.Configure(nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := p.Check(ctx); err != nil {
		t.Logf("Check: %v", err)
	}
	err := p.Send(ctx, notify.Event{
		Kind:  notify.KindTest,
		Title: "ymcp live test",
		Body:  "A click opens the ymcp repository.",
		URL:   "https://github.com/xlsft/ymcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := any(p).(io.Closer); ok {
		// Linux handles the click in this process: give it a moment.
		if wait := os.Getenv("YMCP_NOTIFY_LIVE_WAIT"); wait != "" {
			if d, err := time.ParseDuration(wait); err == nil {
				time.Sleep(d)
			}
		}
		c.Close()
	}
	t.Log("sent; look for the notification")
}
