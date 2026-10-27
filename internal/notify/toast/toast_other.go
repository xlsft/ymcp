//go:build !darwin && !windows && !linux

package toast

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/xlsft/ymcp/internal/notify"
)

type provider struct{}

func newProvider() *provider { return &provider{} }

func (p *provider) Name() string { return Name }

// EnabledByDefault is false: there is nothing to show notifications with.
func (p *provider) EnabledByDefault() bool { return false }

func (p *provider) Configure(raw json.RawMessage) error {
	_, err := parseSettings(raw)
	return err
}

func (p *provider) Check(context.Context) error { return errUnsupported }

func (p *provider) Send(context.Context, notify.Event) error { return errUnsupported }

var errUnsupported = fmt.Errorf("system notifications are not supported on %s", runtime.GOOS)
