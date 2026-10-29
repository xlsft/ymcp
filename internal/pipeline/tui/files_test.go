package tui

import "testing"

func TestSystemFilesOffOverSSH(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	if systemFiles(env(nil)) == nil {
		t.Error("no dialog on a desktop")
	}
	for _, k := range []string{"SSH_CONNECTION", "SSH_TTY"} {
		if f := systemFiles(env(map[string]string{k: "x"})); f != nil {
			t.Errorf("%s set: dialog %T", k, f)
		}
	}
}
