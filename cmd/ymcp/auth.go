package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"golang.org/x/term"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/youtrack"
)

func interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func auth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "YouTrack base URL")
	loginFlag := fs.String("login", "", "login")
	proxyFlag := fs.String("proxy", "", `proxy: empty for HTTP(S)_PROXY, "none" for direct, or a URL`)
	passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *urlFlag != "" {
		cfg.URL = *urlFlag
	}
	if *loginFlag != "" {
		cfg.Login = *loginFlag
	}
	if *proxyFlag != "" {
		cfg.Proxy = *proxyFlag
	}
	if *passwordStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read password: %w", err)
		}
		cfg.Password = strings.TrimRight(line, "\r\n")
	}
	_, err = authenticate(ctx, cfg, !*passwordStdin)
	return err
}

// authenticate asks for whatever is missing (or everything, when ask is
// set and the terminal is interactive), verifies the login and saves it.
func authenticate(ctx context.Context, cfg config.Config, ask bool) (config.Config, error) {
	if ask && interactive() {
		if err := credentialsForm(&cfg).Run(); err != nil {
			return cfg, err
		}
	}
	if !cfg.Complete() {
		return cfg, errors.New("URL, login and password are required (run in a terminal, or pass --url, --login and --password-stdin)")
	}
	cfg.URL = strings.TrimRight(strings.TrimSpace(cfg.URL), "/")

	var me user
	var verifyErr error
	verify := func() {
		yt, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy})
		if err == nil {
			me, err = currentUser(ctx, yt)
		}
		verifyErr = err
	}
	if interactive() {
		_ = spinner.New().Title(" Logging in to " + cfg.URL + "…").Action(verify).Run()
	} else {
		verify()
	}
	if verifyErr != nil {
		return cfg, fmt.Errorf("login failed, nothing saved: %w", verifyErr)
	}

	store, err := config.Save(cfg)
	if err != nil {
		return cfg, err
	}
	path, _ := config.Path()
	fmt.Println(styleOK.Render("✓") + " Logged in to " + cfg.URL + " as " + styleBold.Render(me.Login) + " (" + me.FullName + ")")
	if store == "keychain" {
		fmt.Println(styleOK.Render("✓") + " Password saved to the system keychain, settings to " + path)
	} else {
		fmt.Println(styleWarn.Render("!") + " No system keychain available: the password is saved in " + path + " (mode 0600)")
	}
	return cfg, nil
}

func credentialsForm(cfg *config.Config) *huh.Form {
	if cfg.URL == "" {
		cfg.URL = "https://"
	}
	return huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("YouTrack connection").
				Description("ymcp signs in like the YouTrack web UI.\nThe password is kept in your system keychain."),
			huh.NewInput().
				Title("YouTrack URL").
				Placeholder("https://youtrack.example.com").
				Value(&cfg.URL).
				Validate(func(s string) error {
					u, err := url.Parse(strings.TrimSpace(s))
					if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
						return errors.New("enter a full URL, e.g. https://youtrack.example.com")
					}
					return nil
				}),
			huh.NewInput().
				Title("Login").
				Value(&cfg.Login).
				Validate(nonEmpty("login")),
			huh.NewInput().
				Title("Password").
				EchoMode(huh.EchoModePassword).
				Value(&cfg.Password).
				Validate(nonEmpty("password")),
			huh.NewInput().
				Title("Proxy").
				Description(`Empty: use HTTP(S)_PROXY · "none": connect directly · or a proxy URL`).
				Value(&cfg.Proxy),
		),
	).WithTheme(huh.ThemeBase16())
}

func nonEmpty(what string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", what)
		}
		return nil
	}
}
