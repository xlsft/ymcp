package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	charmlog "github.com/charmbracelet/log"
	"golang.org/x/term"
)

// maxLogSize is when the log file is rotated to <file>.1 on startup.
const maxLogSize = 5 << 20

// logFilePath is where serve and call append their log; YMCP_LOG_FILE
// overrides it and "off" disables the file.
func logFilePath() (string, error) {
	if p := os.Getenv("YMCP_LOG_FILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "ymcp", "ymcp.log"), nil
	case "windows":
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			dir = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(dir, "ymcp", "logs", "ymcp.log"), nil
	default:
		dir := os.Getenv("XDG_STATE_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(dir, "ymcp", "ymcp.log"), nil
	}
}

// logLevel is info, or debug with -v; YMCP_LOG_LEVEL (debug, info, warn,
// error) overrides both.
func logLevel(verbose bool) slog.Level {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	if v := os.Getenv("YMCP_LOG_LEVEL"); v != "" {
		_ = level.UnmarshalText([]byte(v))
	}
	return level
}

// newLogger logs to stderr (colourful on a terminal) and, when withFile is
// set, also appends to the log file. stdout is never used: it carries the
// stdio transport. The returned function closes the file.
func newLogger(level slog.Level, withFile bool) (*slog.Logger, string, func()) {
	var stderr slog.Handler
	if term.IsTerminal(int(os.Stderr.Fd())) {
		stderr = charmlog.NewWithOptions(os.Stderr, charmlog.Options{
			Level:           charmlog.Level(level),
			ReportTimestamp: true,
			TimeFormat:      time.TimeOnly,
		})
	} else {
		stderr = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	if !withFile {
		return slog.New(stderr), "", func() {}
	}

	path, err := logFilePath()
	if err != nil || path == "off" {
		return slog.New(stderr), "", func() {}
	}
	f, err := openLogFile(path)
	if err != nil {
		l := slog.New(stderr)
		l.Warn("can't open log file", "path", path, "err", err)
		return l, "", func() {}
	}
	// Several clients may run ymcp at once; pid tells their lines apart.
	file := slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}).WithAttrs([]slog.Attr{slog.Int("pid", os.Getpid())})
	return slog.New(fanout{stderr, file}), path, func() { f.Close() }
}

func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// atLeast drops records below min, to quiet library loggers.
type atLeast struct {
	slog.Handler
	min slog.Level
}

func (h atLeast) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h atLeast) WithAttrs(attrs []slog.Attr) slog.Handler {
	return atLeast{h.Handler.WithAttrs(attrs), h.min}
}

func (h atLeast) WithGroup(name string) slog.Handler {
	return atLeast{h.Handler.WithGroup(name), h.min}
}

// fanout sends every record to all handlers.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// logs prints the log file, following it with -f.
func logs(ctx context.Context, args []string) error {
	follow, lines := false, 50
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-f" || a == "--follow":
			follow = true
		case a == "-n" && i+1 < len(args):
			fmt.Sscan(args[i+1], &lines)
			i++
		case a == "--path":
			p, err := logFilePath()
			if err != nil {
				return err
			}
			fmt.Println(p)
			return nil
		default:
			return fmt.Errorf("usage: ymcp logs [-f] [-n lines] [--path]")
		}
	}
	path, err := logFilePath()
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s; it appears once an MCP client starts ymcp", path)
	}
	if err != nil {
		return err
	}
	defer f.Close()

	if err := printTail(f, lines); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	fmt.Fprintln(os.Stderr, styleDim.Render("— following "+path+", ctrl+c to stop"))
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
		}
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(300 * time.Millisecond):
			}
			// Start over if the file was rotated.
			if info, err := os.Stat(path); err == nil {
				if pos, _ := f.Seek(0, io.SeekCurrent); info.Size() < pos {
					f.Close()
					if f, err = os.Open(path); err != nil {
						return err
					}
				}
			}
		}
	}
}

// printTail prints the last n lines of f and leaves it positioned at the end.
func printTail(f *os.File, n int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const window = 256 << 10
	start := max(info.Size()-window, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is likely cut
	}
	for _, l := range lines[max(len(lines)-n, 0):] {
		if l != "" {
			fmt.Println(l)
		}
	}
	return nil
}
