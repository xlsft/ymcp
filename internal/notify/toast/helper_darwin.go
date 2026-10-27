package toast

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// The helper is the code that runs inside the wrapper bundle, talking to
// UNUserNotificationCenter through purego. It runs an NSApplication on the
// main thread and exits the process from a callback when done.

const (
	postDeadline   = 2 * time.Minute // a first post may wait for the permission prompt
	statusDeadline = 10 * time.Second
	clickDeadline  = 10 * time.Second
)

// HelperMain runs `ymcp notify post|status` inside the wrapper bundle:
//
//	post --title T --body B --url U --id ID [--sound=false]   show a notification
//	status                                                    print the authorization as JSON
//
// It exits the process when done; it returns only an error found before
// talking to macOS.
func HelperMain(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ymcp notify post|status")
	}
	exe, _ := os.Executable()
	if !inBundle(exe) {
		return fmt.Errorf("ymcp notify only runs inside %s; send notifications with `ymcp pipeline notify test`", bundleName)
	}
	switch args[0] {
	case "status":
		if len(args) > 1 {
			return errors.New("usage: ymcp notify status")
		}
		runApp(helperJob{mode: "status"})
	case "post":
		fs := flag.NewFlagSet("notify post", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var j helperJob
		j.mode = "post"
		fs.StringVar(&j.title, "title", "ymcp", "")
		fs.StringVar(&j.body, "body", "", "")
		fs.StringVar(&j.url, "url", "", "")
		fs.StringVar(&j.id, "id", "", "")
		fs.BoolVar(&j.sound, "sound", true, "")
		if err := fs.Parse(args[1:]); err != nil {
			return fmt.Errorf("notify post: %w", err)
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("notify post: unexpected argument %q", fs.Arg(0))
		}
		if j.id == "" {
			j.id = fmt.Sprintf("ymcp-%d", time.Now().UnixNano())
		}
		runApp(j)
	default:
		return fmt.Errorf("unknown notify command %q; usage: ymcp notify post|status", args[0])
	}
	return nil
}

// ClickLaunch reports whether this process is the wrapper bundle launched
// by macOS for a click on one of its notifications (the bundle's binary
// with no command). The caller then runs HandleClick before anything else.
func ClickLaunch() bool {
	exe, err := os.Executable()
	return err == nil && inBundle(exe) && clickArgs(os.Args[1:])
}

// HandleClick waits for the notification response macOS relaunched the
// bundle for, opens its URL and exits the process.
func HandleClick() {
	runApp(helperJob{mode: "click"})
}

type helperJob struct {
	mode                 string // post, status or click
	title, body, url, id string
	sound                bool
}

var sel = objc.RegisterName

func nsString(s string) objc.ID {
	return objc.ID(objc.GetClass("NSString")).Send(sel("stringWithUTF8String:"), s)
}

func goString(id objc.ID) string {
	if id == 0 {
		return ""
	}
	p := objc.Send[*byte](id, sel("UTF8String"))
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}

func describe(id objc.ID) string {
	if id == 0 {
		return ""
	}
	return goString(id.Send(sel("localizedDescription")))
}

// callBlock invokes a block Objective-C handed us (not one made by
// objc.NewBlock): Block_literal is {isa; int flags; int reserved; invoke; …},
// so invoke is at offset 16.
func callBlock(b objc.Block, args ...uintptr) {
	lit := *(*unsafe.Pointer)(unsafe.Pointer(&b)) // the block's address, as a pointer
	invoke := *(*uintptr)(unsafe.Add(lit, 16))
	purego.SyscallN(invoke, append([]uintptr{uintptr(b)}, args...)...)
}

var authNames = map[int]string{0: "notDetermined", 1: "denied", 2: "authorized", 3: "provisional", 4: "ephemeral"}

// UNNotificationPresentationOptions and UNAuthorizationOptions bits.
const (
	presentSound  = 1 << 1
	presentList   = 1 << 3
	presentBanner = 1 << 4
	authSound     = 1 << 1
	authAlert     = 1 << 2
)

var exitOnce sync.Once

// exit ends the helper. The first caller wins (a deadline and a callback
// may race); later callers block until the process is gone.
func exit(code int, stderr string) {
	exitOnce.Do(func() {
		if stderr != "" {
			fmt.Fprintln(os.Stderr, stderr)
		}
		os.Exit(code)
	})
}

func runApp(j helperJob) {
	for _, fw := range []string{
		"/System/Library/Frameworks/AppKit.framework/AppKit",
		"/System/Library/Frameworks/UserNotifications.framework/UserNotifications",
	} {
		if _, err := purego.Dlopen(fw, purego.RTLD_GLOBAL|purego.RTLD_NOW); err != nil {
			exit(1, fmt.Sprintf("loading %s: %v", filepath.Base(fw), err))
		}
	}
	bundle := objc.ID(objc.GetClass("NSBundle")).Send(sel("mainBundle"))
	if goString(bundle.Send(sel("bundleIdentifier"))) != bundleID {
		exit(1, "not running as the "+bundleID+" bundle")
	}
	center := objc.ID(objc.GetClass("UNUserNotificationCenter")).Send(sel("currentNotificationCenter"))

	deadline := map[string]time.Duration{"post": postDeadline, "status": statusDeadline, "click": clickDeadline}[j.mode]
	go func() {
		time.Sleep(deadline)
		if j.mode == "click" {
			exit(0, "")
		}
		exit(3, fmt.Sprintf("macOS did not answer within %s", deadline))
	}()

	willPresent := func(_ objc.ID, _ objc.SEL, _, _ objc.ID, done objc.Block) {
		opts := uintptr(presentBanner | presentList)
		if j.sound {
			opts |= presentSound
		}
		callBlock(done, opts)
	}
	didReceive := func(_ objc.ID, _ objc.SEL, _, resp objc.ID, done objc.Block) {
		info := resp.Send(sel("notification")).Send(sel("request")).Send(sel("content")).Send(sel("userInfo"))
		link := goString(info.Send(sel("objectForKey:"), nsString("url")))
		opened := false
		if openable(link) {
			nsurl := objc.ID(objc.GetClass("NSURL")).Send(sel("URLWithString:"), nsString(link))
			if nsurl != 0 {
				opened = objc.Send[bool](objc.ID(objc.GetClass("NSWorkspace")).Send(sel("sharedWorkspace")), sel("openURL:"), nsurl)
			}
		}
		clickLog("click url=%q opened=%v", link, opened)
		callBlock(done)
		go func() { time.Sleep(300 * time.Millisecond); exit(0, "") }()
	}
	willFinish := func(self objc.ID, _ objc.SEL, _ objc.ID) {
		// Before launch completes, so the response a click relaunched us
		// for is delivered to this delegate.
		center.Send(sel("setDelegate:"), self)
	}
	didFinish := func(_ objc.ID, _ objc.SEL, _ objc.ID) {
		switch j.mode {
		case "status":
			center.Send(sel("getNotificationSettingsWithCompletionHandler:"), objc.NewBlock(func(_ objc.Block, s objc.ID) {
				st := authStatus{
					Authorization: authNames[objc.Send[int](s, sel("authorizationStatus"))],
					AlertStyle:    objc.Send[int](s, sel("alertStyle")),
				}
				out, _ := json.Marshal(st)
				os.Stdout.Write(append(out, '\n'))
				exit(0, "")
			}))
		case "post":
			center.Send(sel("requestAuthorizationWithOptions:completionHandler:"), uint(authAlert|authSound),
				objc.NewBlock(func(_ objc.Block, granted bool, err objc.ID) {
					if !granted {
						msg := "notifications from ymcp are turned off; allow them in " + settingsHint
						if d := describe(err); d != "" {
							msg += " (" + d + ")"
						}
						exit(1, msg)
					}
					post(center, j)
				}))
		}
	}

	cls, err := objc.RegisterClass("YMCPNotifyDelegate", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
		{Cmd: sel("applicationWillFinishLaunching:"), Fn: willFinish},
		{Cmd: sel("applicationDidFinishLaunching:"), Fn: didFinish},
		{Cmd: sel("userNotificationCenter:willPresentNotification:withCompletionHandler:"), Fn: willPresent},
		{Cmd: sel("userNotificationCenter:didReceiveNotificationResponse:withCompletionHandler:"), Fn: didReceive},
	})
	if err != nil {
		exit(1, fmt.Sprintf("registering the notification delegate: %v", err))
	}
	delegate := objc.ID(cls).Send(sel("alloc")).Send(sel("init"))
	app := objc.ID(objc.GetClass("NSApplication")).Send(sel("sharedApplication"))
	app.Send(sel("setActivationPolicy:"), 1) // accessory: no Dock icon
	app.Send(sel("setDelegate:"), delegate)
	app.Send(sel("run"))
	exit(1, "the application loop ended unexpectedly")
}

func post(center objc.ID, j helperJob) {
	c := objc.ID(objc.GetClass("UNMutableNotificationContent")).Send(sel("alloc")).Send(sel("init"))
	c.Send(sel("setTitle:"), nsString(j.title))
	c.Send(sel("setBody:"), nsString(j.body))
	if j.sound {
		c.Send(sel("setSound:"), objc.ID(objc.GetClass("UNNotificationSound")).Send(sel("defaultSound")))
	}
	if j.url != "" {
		c.Send(sel("setUserInfo:"), objc.ID(objc.GetClass("NSDictionary")).Send(sel("dictionaryWithObject:forKey:"), nsString(j.url), nsString("url")))
	}
	req := objc.ID(objc.GetClass("UNNotificationRequest")).Send(sel("requestWithIdentifier:content:trigger:"), nsString(j.id), c, objc.ID(0))
	center.Send(sel("addNotificationRequest:withCompletionHandler:"), req, objc.NewBlock(func(_ objc.Block, err objc.ID) {
		if err != 0 {
			exit(1, "macOS refused the notification: "+describe(err))
		}
		exit(0, "")
	}))
}

// clickLog notes what a click did in notify.log next to the bundle; there
// is no terminal to report to. The log is kept small.
func clickLog(format string, args ...any) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	path := filepath.Join(filepath.Dir(exe), "..", "..", "..", "notify.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 64<<10 {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
