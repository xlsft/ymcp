//go:build !darwin

package toast

import "errors"

// HelperMain is the macOS notification helper (`ymcp notify post|status`);
// elsewhere there is nothing to run.
func HelperMain(args []string) error {
	return errors.New("ymcp notify is only used on macOS; send a test notification with `ymcp pipeline notify test`")
}

// ClickLaunch reports whether macOS relaunched the wrapper bundle for a
// click; never outside macOS.
func ClickLaunch() bool { return false }

// HandleClick handles a macOS notification click; a no-op elsewhere.
func HandleClick() {}
