package main

import "github.com/xlsft/ymcp/internal/notify/toast"

// The hidden `ymcp notify` command and the click hook belong to the macOS
// notification wrapper (~/Library/Application Support/ymcp/ymcp.app, see
// internal/notify/toast): the wrapper's copy of ymcp runs `ymcp notify post`
// to show a notification, and macOS relaunches the wrapper with no
// arguments when one is clicked.

// clickRelaunch handles a launch of the wrapper by macOS for a click: it
// opens the notification's URL and reports true, and main exits. Anywhere
// else it does nothing and reports false.
func clickRelaunch() bool {
	if !toast.ClickLaunch() {
		return false
	}
	toast.HandleClick()
	return true
}

// notifyHelper runs `ymcp notify post|status` inside the wrapper; it exits
// the process when done.
func notifyHelper(args []string) error {
	return toast.HelperMain(args)
}
