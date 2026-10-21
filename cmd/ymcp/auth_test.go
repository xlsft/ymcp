package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack/youtracktest"
)

func TestAuthenticateSavesOnlyAfterLogin(t *testing.T) {
	keyring.MockInit()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("YMCP_CONFIG", path)
	for _, k := range []string{"YOUTRACK_URL", "YOUTRACK_LOGIN", "YOUTRACK_PASSWORD", "YOUTRACK_PROXY"} {
		t.Setenv(k, "")
	}
	f := youtracktest.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"login":"alice","fullName":"Alice"}`))
	}))

	bad := config.Config{URL: f.URL, Login: youtracktest.Login, Password: "wrong", Proxy: "none"}
	if _, err := authenticate(context.Background(), bad, false); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config saved after a failed login")
	}

	good := bad
	good.Password = youtracktest.Password
	good.URL = f.URL + "/"
	if _, err := authenticate(context.Background(), good, false); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil || got.URL != f.URL || got.Password != youtracktest.Password {
		t.Errorf("stored = %+v, %v", got, err)
	}
}
