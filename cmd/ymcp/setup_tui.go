package main

import (
	"context"
	"runtime"
	"slices"

	"github.com/xlsft/ymcp/internal/clients"
	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/tui/wizard"
	setupui "github.com/xlsft/ymcp/internal/setup"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// setupTUI opens the setup TUI, on the --profile profile when given.
func setupTUI(ctx context.Context) error {
	env, err := clients.System(runtime.GOOS)
	if err != nil {
		return err
	}
	return setupui.Run(ctx, setupui.Options{Deps: setupDeps(env), Open: profile})
}

// setupDeps backs the setup TUI with the config store and the clients of
// env.
func setupDeps(env clients.Env) setupui.Deps {
	return setupui.Deps{
		Profiles: func() ([]setupui.Profile, error) {
			list, err := config.Profiles()
			if err != nil {
				return nil, err
			}
			detected, _ := detectClients(env)
			out := make([]setupui.Profile, 0, len(list))
			for _, c := range list {
				p := setupui.Profile{Config: c}
				for _, cl := range detected {
					if cl.Installed(env, c.Name) {
						p.Clients = append(p.Clients, cl.Name)
					}
				}
				out = append(out, p)
			}
			return out, nil
		},
		Load: config.Load,
		Clients: func(key string) []wizard.Client {
			detected, _ := detectClients(env)
			out := make([]wizard.Client, 0, len(detected))
			for _, c := range detected {
				running := c.App() != "" && c.Running(env)
				out = append(out, wizard.Client{
					ID: c.ID, Name: c.Name, Note: c.Note,
					Registered: key != "" && c.Installed(env, key),
					Running:    running,
					Inside:     running && insideApp(c.App()),
				})
			}
			return out
		},
		Save: func(_ context.Context, old string, p wizard.ProfileDraft) ([]string, error) {
			cfg := p.Config
			if old != cfg.Name {
				if list, err := config.Profiles(); err != nil {
					return nil, err
				} else if slices.ContainsFunc(list, func(c config.Config) bool { return c.Name == cfg.Name }) {
					return nil, errExists(cfg.Name)
				}
			}
			store, err := config.Save(cfg, old)
			if err != nil {
				return nil, err
			}
			lines := []string{styleOK.Render("✓") + " Saved profile " + styleBold.Render(cfg.Title())}
			if old != "" && old != cfg.Name {
				lines[0] += styleDim.Render(", renamed from " + old)
			}
			if store == "file" {
				path, _ := config.Path()
				lines = append(lines, styleWarn.Render("!")+" No system keychain available: the password is saved in "+path+" (mode 0600)")
			}
			reg, _, err := register(env, cfg, old, p.Clients, p.Quit)
			if err != nil {
				reg = append(reg, styleWarn.Render("✗")+" Not registered in any client: "+err.Error())
			}
			return append(lines, reg...), nil
		},
		Remove: func(_ context.Context, key string) ([]string, error) {
			lines, _, err := unregister(env, key, true)
			return lines, err
		},
		Wizard: wizard.ProfileDeps{
			SignIn: func(ctx context.Context, c config.Config) (string, error) {
				yt, err := youtrack.New(youtrack.Options{BaseURL: c.URL, Login: c.Login, Password: c.Password, Proxy: c.Proxy})
				if err != nil {
					return "", err
				}
				me, err := currentUser(ctx, yt)
				return me.Login, err
			},
			Command: func(key string) string {
				bin, err := binaryPath()
				if err != nil {
					bin = "ymcp"
				}
				return bin + " --profile " + key
			},
		},
	}
}

type errExists string

func (e errExists) Error() string {
	return "a profile " + string(e) + " already exists; pick another name"
}
