package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xlsft/ymcp/internal/config"
	"github.com/xlsft/ymcp/internal/pipeline/store"
	"github.com/xlsft/ymcp/internal/pipeline/tui/termimg"
	"github.com/xlsft/ymcp/internal/pipeline/tui/views"
	"github.com/xlsft/ymcp/internal/pipeline/vcs"
	"github.com/xlsft/ymcp/internal/pipeline/ytwatch"
	"github.com/xlsft/ymcp/internal/youtrack"
)

// YouTrack is the TUI's YouTrack, one signed-in client per profile: the
// board's backlog search (views.Backlog) and the run card's issue and
// comments (views.Issues). Comments are posted as the profile's user, so a
// runner of that profile reads them like any comment of its user.
type YouTrack struct {
	// Load reads a profile; nil uses config.Load.
	Load func(profile string) (config.Config, error)

	mu      sync.Mutex
	clients map[string]*youtrack.Client
}

func (y *YouTrack) client(profile string) (*youtrack.Client, error) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if c := y.clients[profile]; c != nil {
		return c, nil
	}
	load := y.Load
	if load == nil {
		load = config.Load
	}
	cfg, err := load(profile)
	if err != nil {
		return nil, err
	}
	if !cfg.Complete() {
		return nil, errors.New("profile " + profile + " is not configured")
	}
	c, err := youtrack.New(youtrack.Options{BaseURL: cfg.URL, Login: cfg.Login, Password: cfg.Password, Proxy: cfg.Proxy})
	if err != nil {
		return nil, err
	}
	if y.clients == nil {
		y.clients = map[string]*youtrack.Client{}
	}
	y.clients[profile] = c
	return c, nil
}

// Search runs query in the YouTrack of profile.
func (y *YouTrack) Search(ctx context.Context, profile, query string) ([]ytwatch.Issue, error) {
	c, err := y.client(profile)
	if err != nil {
		return nil, err
	}
	return ytwatch.New(c).Search(ctx, query)
}

// Issue reads an issue with its description and custom fields.
func (y *YouTrack) Issue(ctx context.Context, profile, issue string) (ytwatch.Issue, error) {
	c, err := y.client(profile)
	if err != nil {
		return ytwatch.Issue{}, err
	}
	return ytwatch.New(c).Issue(ctx, issue)
}

const commentFields = "id,text,created,updated,deleted,author(login,fullName),attachments(name,url,mimeType,removed)"

// commentPage is how many comments one request reads; commentPages caps
// the pages.
const (
	commentPage  = 200
	commentPages = 25
)

type apiComment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
	Updated *int64 `json:"updated"`
	Deleted bool   `json:"deleted"`
	Author  *struct {
		Login    string `json:"login"`
		FullName string `json:"fullName"`
	} `json:"author"`
	Attachments []apiAttachment `json:"attachments"`
}

type apiAttachment struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	Removed  bool   `json:"removed"`
	Comment  *struct {
		ID string `json:"id"`
	} `json:"comment"`
}

// files are the attachments still there.
func files(list []apiAttachment) []views.Attachment {
	var out []views.Attachment
	for _, a := range list {
		if !a.Removed && a.URL != "" {
			out = append(out, views.Attachment{Name: a.Name, URL: a.URL, MimeType: a.MimeType})
		}
	}
	return out
}

func (a apiComment) comment() views.IssueComment {
	c := views.IssueComment{ID: a.ID, Text: a.Text, Created: time.UnixMilli(a.Created)}
	if a.Updated != nil && *a.Updated > 0 {
		c.Updated = time.UnixMilli(*a.Updated)
	}
	if a.Author != nil {
		c.Author, c.AuthorName = a.Author.Login, a.Author.FullName
	}
	c.Files = files(a.Attachments)
	return c
}

func commentsPath(issue string) string { return "issues/" + url.PathEscape(issue) + "/comments" }

// Comments lists an issue's comments, oldest first, without deleted ones.
func (y *YouTrack) Comments(ctx context.Context, profile, issue string) ([]views.IssueComment, error) {
	c, err := y.client(profile)
	if err != nil {
		return nil, err
	}
	var out []views.IssueComment
	for page := range commentPages {
		q := youtrack.Fields(commentFields)
		q.Set("$top", strconv.Itoa(commentPage))
		q.Set("$skip", strconv.Itoa(page*commentPage))
		var raw []apiComment
		if err := c.Get(ctx, commentsPath(issue), q, &raw); err != nil {
			return nil, fmt.Errorf("comments of %s: %w", issue, err)
		}
		for _, a := range raw {
			if !a.Deleted {
				out = append(out, a.comment())
			}
		}
		if len(raw) < commentPage {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// AddComment posts a comment on an issue as the profile's user.
func (y *YouTrack) AddComment(ctx context.Context, profile, issue, text string) (views.IssueComment, error) {
	c, err := y.client(profile)
	if err != nil {
		return views.IssueComment{}, err
	}
	var a apiComment
	if err := c.Post(ctx, commentsPath(issue), youtrack.Fields(commentFields), map[string]string{"text": text}, &a); err != nil {
		return views.IssueComment{}, fmt.Errorf("commenting on %s: %w", issue, err)
	}
	return a.comment(), nil
}

// CodeHost reads the conversation of a run's pull or merge request through
// the run's strategy (views.Changes), one strategy per repository.
type CodeHost struct {
	// New makes the strategy of a run's repository ("host/owner/name");
	// nil uses vcs.ParseRemote, vcs.TokenFor and vcs.New, without a token
	// when none is found.
	New func(ctx context.Context, kind vcs.Kind, repo string) (vcs.Strategy, error)

	mu         sync.Mutex
	strategies map[string]vcs.Strategy
}

func newStrategy(ctx context.Context, kind vcs.Kind, repo string) (vcs.Strategy, error) {
	r, err := vcs.ParseRemote("https://" + repo)
	if err != nil {
		return nil, err
	}
	auth, err := vcs.TokenFor(ctx, r)
	if err != nil {
		auth = vcs.Auth{} // public repositories still read
	}
	return vcs.New(kind, r, auth)
}

func (h *CodeHost) strategy(ctx context.Context, kind vcs.Kind, repo string) (vcs.Strategy, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := string(kind) + " " + repo
	if s := h.strategies[key]; s != nil {
		return s, nil
	}
	mk := h.New
	if mk == nil {
		mk = newStrategy
	}
	s, err := mk(ctx, kind, repo)
	if err != nil {
		return nil, err
	}
	if h.strategies == nil {
		h.strategies = map[string]vcs.Strategy{}
	}
	h.strategies[key] = s
	return s, nil
}

// Comments returns every comment on the run's pull or merge request,
// oldest first.
func (h *CodeHost) Comments(ctx context.Context, run store.Run) ([]vcs.Comment, error) {
	kind := vcs.Kind(run.VCSKind)
	if run.PRNumber == 0 || run.VCSRepo == "" || kind == "" || kind == vcs.KindBranch {
		return nil, fmt.Errorf("run #%d has no pull or merge request", run.Number)
	}
	s, err := h.strategy(ctx, kind, run.VCSRepo)
	if err != nil {
		return nil, err
	}
	cs, _, err := s.Comments(ctx, run.PRNumber, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Created.Before(cs[j].Created) })
	return cs, nil
}

// Users lists the active users of profile's YouTrack whose name, login or
// email match query, for @mentions.
func (y *YouTrack) Users(ctx context.Context, profile, query string) ([]views.Person, error) {
	c, err := y.client(profile)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Login    string `json:"login"`
		FullName string `json:"fullName"`
		Email    string `json:"email"`
		Banned   bool   `json:"banned"`
	}
	q := youtrack.Fields("login,fullName,email,banned")
	q.Set("query", query)
	q.Set("$top", "10")
	if err := c.Get(ctx, "users", q, &raw); err != nil {
		return nil, fmt.Errorf("users: %w", err)
	}
	out := make([]views.Person, 0, len(raw))
	for _, u := range raw {
		if !u.Banned && u.Login != "" {
			out = append(out, views.Person{Login: u.Login, FullName: u.FullName, Email: u.Email})
		}
	}
	return out, nil
}

// Attachments lists the files attached to the issue itself; those of its
// comments come with the comments.
func (y *YouTrack) Attachments(ctx context.Context, profile, issue string) ([]views.Attachment, error) {
	c, err := y.client(profile)
	if err != nil {
		return nil, err
	}
	var raw []apiAttachment
	q := youtrack.Fields("name,url,mimeType,removed,comment(id)")
	q.Set("$top", "-1")
	if err := c.Get(ctx, "issues/"+url.PathEscape(issue)+"/attachments", q, &raw); err != nil {
		return nil, fmt.Errorf("attachments of %s: %w", issue, err)
	}
	own := raw[:0]
	for _, a := range raw {
		if a.Comment == nil {
			own = append(own, a)
		}
	}
	return files(own), nil
}

// Fetch downloads a picture: YouTrack's own files as the profile's user,
// anything else as it is.
func (y *YouTrack) Fetch(ctx context.Context, profile, fileURL string) ([]byte, error) {
	c, err := y.client(profile)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(c.BaseURL(), "/")
	switch {
	case strings.HasPrefix(fileURL, "/"):
		data, _, err := c.Download(ctx, fileURL)
		return data, err
	case base != "" && strings.HasPrefix(fileURL, base+"/"):
		data, _, err := c.Download(ctx, strings.TrimPrefix(fileURL, base))
		return data, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", fileURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, termimg.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	return data, nil
}
