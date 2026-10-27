package spec

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNotificationsRoundTrip(t *testing.T) {
	dir := setup(t)
	in := minimal("backend")
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "backend.json"))
	if strings.Contains(string(data), "notifications") {
		t.Errorf("unset notifications written: %s", data)
	}

	in.Notifications = &Notifications{Providers: []string{"toast"}, Events: []string{"needs_input", "review_ready"}}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := Load("backend")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Notifications, in.Notifications) {
		t.Errorf("notifications = %+v", got.Notifications)
	}
}

func TestNotificationsValidate(t *testing.T) {
	for _, c := range []struct {
		n    *Notifications
		want string // substring of the problem; "" for none
	}{
		{nil, ""},
		{&Notifications{}, ""},
		{&Notifications{Providers: []string{"toast"}, Events: NotificationEvents}, ""},
		{&Notifications{Events: []string{"needs_input", "party"}}, `unknown event "party"`},
		{&Notifications{Events: []string{"test"}}, `unknown event "test"`},
		{&Notifications{Providers: []string{" "}}, "empty name"},
	} {
		p := minimal("backend")
		p.Notifications = c.n
		err := p.Validate()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v", c.n, err)
		}
	}
}
