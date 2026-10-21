package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogLevel(t *testing.T) {
	t.Setenv("YMCP_LOG_LEVEL", "")
	if logLevel(false) != slog.LevelInfo || logLevel(true) != slog.LevelDebug {
		t.Error("default levels wrong")
	}
	t.Setenv("YMCP_LOG_LEVEL", "warn")
	if logLevel(true) != slog.LevelWarn {
		t.Error("YMCP_LOG_LEVEL ignored")
	}
}

func TestPrettyHandler(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(&prettyHandler{w: &buf, level: slog.LevelInfo}).With("pid", 7).WithGroup("g")
	l.Debug("hidden")
	l.Info("signed in", "login", "alice")
	l.Warn("careful")
	l.Error("broken")

	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Error("debug line written at info level")
	}
	plain := stripANSI(out)
	for _, want := range []string{"INFO  signed in pid=7 g.login=alice", "WARN  careful", "ERROR broken"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in\n%s", want, plain)
		}
	}
	// Only SGR colour codes: anything else (e.g. OSC queries) would make
	// the terminal answer into stdin.
	if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x1b[6n") {
		t.Errorf("handler writes terminal queries: %q", out)
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestFanoutAndAtLeast(t *testing.T) {
	var a, b bytes.Buffer
	h := fanout{
		slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelDebug}),
		slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn}),
	}
	l := slog.New(h).With("k", "v").WithGroup("g")
	l.Info("info line")
	l.Warn("warn line")
	if !strings.Contains(a.String(), "info line") || !strings.Contains(a.String(), "k=v") {
		t.Errorf("first handler: %s", a.String())
	}
	if strings.Contains(b.String(), "info line") || !strings.Contains(b.String(), "warn line") {
		t.Errorf("second handler: %s", b.String())
	}

	var c bytes.Buffer
	quiet := slog.New(atLeast{slog.NewTextHandler(&c, nil), slog.LevelWarn}).With("x", 1).WithGroup("q")
	quiet.Info("dropped")
	quiet.Warn("kept")
	if strings.Contains(c.String(), "dropped") || !strings.Contains(c.String(), "kept") {
		t.Errorf("atLeast: %s", c.String())
	}
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("fanout should be enabled when any handler is")
	}
}

func TestNewLoggerWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "ymcp.log")
	t.Setenv("YMCP_LOG_FILE", path)
	logger, got, closeLog := newLogger(slog.LevelInfo, true)
	logger.Info("hello", "n", 1)
	closeLog()
	if got != path {
		t.Errorf("path = %q", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "msg=hello") || !strings.Contains(string(data), fmt.Sprintf("pid=%d", os.Getpid())) {
		t.Errorf("log file: %s", data)
	}

	t.Setenv("YMCP_LOG_FILE", "off")
	if _, got, _ := newLogger(slog.LevelInfo, true); got != "" {
		t.Errorf("off still logs to %q", got)
	}
}

func TestOpenLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ymcp.log")
	os.WriteFile(path, bytes.Repeat([]byte("x"), maxLogSize+1), 0o600)
	f, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if info, _ := os.Stat(path); info.Size() != 0 {
		t.Errorf("log not rotated, size %d", info.Size())
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != maxLogSize+1 {
		t.Errorf("rotated file: %v %v", info, err)
	}
}

func TestLogsCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ymcp.log")
	t.Setenv("YMCP_LOG_FILE", path)

	if err := logs(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "no log yet") {
		t.Errorf("missing file err = %v", err)
	}
	var lines []string
	for i := range 5 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	out, err := captureStdout(t, func() error { return logs(context.Background(), []string{"-n", "2"}) })
	if err != nil || out != "line 3\nline 4\n" {
		t.Errorf("logs -n 2 = %q, %v", out, err)
	}
	out, _ = captureStdout(t, func() error { return logs(context.Background(), []string{"--path"}) })
	if strings.TrimSpace(out) != path {
		t.Errorf("--path = %q", out)
	}
	if err := logs(context.Background(), []string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	}

	// -f prints lines appended later and stops with the context.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(400 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("line 5\n")
		f.Close()
	}()
	out, err = captureStdout(t, func() error { return logs(ctx, []string{"-f", "-n", "1"}) })
	if err != nil || out != "line 4\nline 5\n" {
		t.Errorf("logs -f = %q, %v", out, err)
	}
}

func TestLogFilePathPerOS(t *testing.T) {
	t.Setenv("YMCP_LOG_FILE", "")
	p, err := logFilePath()
	if err != nil || !strings.HasSuffix(p, "ymcp.log") || !strings.Contains(p, "ymcp") {
		t.Errorf("logFilePath = %q, %v", p, err)
	}
}
