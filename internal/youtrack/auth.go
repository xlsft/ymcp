package youtrack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotAuthenticated means Hub rejected the login or the session expired.
var ErrNotAuthenticated = errors.New("youtrack: not authenticated")

// hubAuth obtains short-lived access tokens by logging in to Hub with a
// login and password, the same way the YouTrack web UI does: an interactive
// login sets a Hub session cookie, then the implicit OAuth flow of the
// YouTrack service returns a bearer token in the redirect fragment. Expired
// tokens are refreshed silently from the session; the password is resent
// only when the session itself is gone.
type hubAuth struct {
	baseURL  *url.URL
	login    string
	password string
	http     *http.Client // no redirects, own cookie jar
	now      func() time.Time

	mu        sync.Mutex
	hubURL    *url.URL
	serviceID string
	token     string
	expires   time.Time
}

func newHubAuth(baseURL *url.URL, login, password string, transport http.RoundTripper, timeout time.Duration) *hubAuth {
	jar, _ := cookiejar.New(nil)
	return &hubAuth{
		baseURL:  baseURL,
		login:    login,
		password: password,
		now:      time.Now,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			Jar:       jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Token returns a valid access token, logging in when needed.
func (a *hubAuth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.now().Before(a.expires) {
		return a.token, nil
	}
	if err := a.discover(ctx); err != nil {
		return "", err
	}
	// The Hub session usually outlives the token, so try without the password first.
	if err := a.authorize(ctx); err == nil {
		return a.token, nil
	} else if !errors.Is(err, ErrNotAuthenticated) {
		return "", err
	}
	if err := a.passwordLogin(ctx); err != nil {
		return "", err
	}
	return a.token, nil
}

// Invalidate drops the cached token so the next Token call re-authenticates.
func (a *hubAuth) Invalidate(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.token = ""
	}
}

// discover reads the Hub location and the YouTrack service ID from the
// public frontend config.
func (a *hubAuth) discover(ctx context.Context) error {
	if a.serviceID != "" {
		return nil
	}
	u := a.baseURL.JoinPath("api", "config")
	u.RawQuery = "fields=ring(url,serviceId)"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("youtrack: discover hub: %w", err)
	}
	defer resp.Body.Close()
	var cfg struct {
		Ring struct {
			URL       string `json:"url"`
			ServiceID string `json:"serviceId"`
		} `json:"ring"`
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return fmt.Errorf("youtrack: discover hub: unexpected %s %q response from %s (check URL, VPN and proxy settings)",
			resp.Status, resp.Header.Get("Content-Type"), u.Host)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return fmt.Errorf("youtrack: discover hub: %w", err)
	}
	if cfg.Ring.ServiceID == "" || cfg.Ring.URL == "" {
		return errors.New("youtrack: discover hub: config has no hub URL or service ID")
	}
	hub, err := a.baseURL.Parse(cfg.Ring.URL)
	if err != nil {
		return fmt.Errorf("youtrack: discover hub: %w", err)
	}
	a.hubURL = hub
	a.serviceID = cfg.Ring.ServiceID
	return nil
}

func (a *hubAuth) redirectURI() string {
	return a.baseURL.JoinPath("oauth").String()
}

func (a *hubAuth) oauthQuery(credentials, state string) url.Values {
	return url.Values{
		"response_type":       {"token"},
		"client_id":           {a.serviceID},
		"scope":               {a.serviceID},
		"redirect_uri":        {a.redirectURI()},
		"state":               {state},
		"request_credentials": {credentials},
	}
}

// authorize runs the implicit flow against the current Hub session.
func (a *hubAuth) authorize(ctx context.Context) error {
	state := newState()
	u := a.hubURL.JoinPath("api", "rest", "oauth2", "auth")
	u.RawQuery = a.oauthQuery("silent", state).Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	return a.follow(ctx, req, state)
}

// passwordLogin submits the Hub login form and then completes the flow.
func (a *hubAuth) passwordLogin(ctx context.Context) error {
	state := newState()
	u := a.hubURL.JoinPath("api", "rest", "oauth2", "interactive", "login")
	u.RawQuery = a.oauthQuery("default", state).Encode()
	form := url.Values{"username": {a.login}, "password": {a.password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.follow(ctx, req, state)
}

// follow walks the redirect chain until Hub sends the browser back to the
// YouTrack redirect URI, then takes the token from the URL fragment.
func (a *hubAuth) follow(ctx context.Context, req *http.Request, state string) error {
	redirectURI := a.redirectURI()
	for hop := 0; hop < 10; hop++ {
		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("youtrack: hub login: %w", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode/100 != 3 {
			return fmt.Errorf("youtrack: hub login: unexpected %s at %s", resp.Status, req.URL.Path)
		}
		next, err := req.URL.Parse(resp.Header.Get("Location"))
		if err != nil {
			return fmt.Errorf("youtrack: hub login: bad redirect: %w", err)
		}

		if strings.HasPrefix(next.String(), redirectURI) {
			return a.acceptFragment(next.Fragment, state)
		}
		if strings.Contains(next.Path, "/login/totp") {
			return errors.New("youtrack: hub login: two-factor authentication is not supported")
		}
		if strings.HasSuffix(next.Path, "/auth/login") {
			// Hub sent us back to the login page: wrong credentials or no session.
			q := next.Query()
			msg := q.Get("developer_message")
			if msg == "" {
				msg = q.Get("message")
			}
			if msg == "" {
				msg = "login required"
			}
			return fmt.Errorf("%w: %s", ErrNotAuthenticated, msg)
		}

		req, err = http.NewRequestWithContext(ctx, http.MethodGet, next.String(), nil)
		if err != nil {
			return err
		}
	}
	return errors.New("youtrack: hub login: too many redirects")
}

func (a *hubAuth) acceptFragment(fragment, state string) error {
	q, err := url.ParseQuery(fragment)
	if err != nil {
		return fmt.Errorf("youtrack: hub login: bad redirect fragment: %w", err)
	}
	if e := q.Get("error"); e != "" {
		// access_denied / login_required come back when there is no live session.
		return fmt.Errorf("%w: %s: %s", ErrNotAuthenticated, e, q.Get("error_description"))
	}
	if q.Get("state") != state {
		return errors.New("youtrack: hub login: state mismatch")
	}
	token := q.Get("access_token")
	if token == "" {
		return errors.New("youtrack: hub login: no access token in redirect")
	}
	ttl := time.Hour
	if s, err := strconv.Atoi(q.Get("expires_in")); err == nil && s > 0 {
		ttl = time.Duration(s) * time.Second
	}
	// Refresh a little early so in-flight requests don't race the expiry.
	a.token = token
	a.expires = a.now().Add(ttl - min(ttl/10, time.Minute))
	return nil
}

func newState() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
