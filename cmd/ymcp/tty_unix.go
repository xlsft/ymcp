//go:build !windows

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// drainTerminalReplies discards terminal replies still in the input queue.
//
// At startup Lip Gloss (through termenv) asks the terminal for its
// background colour and cursor position. Terminals that answer the cursor
// query first make termenv give up before the colour reply arrives; that
// reply then sits in the input queue and a form would read it as typed
// text ("]11;rgb:…"). Raw mode makes a partial line readable, so the queue
// is read until it has been quiet for a moment.
func drainTerminalReplies() {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	defer term.Restore(fd, state)

	buf := make([]byte, 256)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var set unix.FdSet
		set.Set(fd)
		tv := unix.NsecToTimeval((150 * time.Millisecond).Nanoseconds())
		n, err := unix.Select(fd+1, &set, nil, nil, &tv)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			return // quiet: nothing left to discard
		}
		if _, err := unix.Read(fd, buf); err != nil {
			return
		}
	}
}
