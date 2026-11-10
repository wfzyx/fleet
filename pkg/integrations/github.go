package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultGitHubAPI is the public GitHub REST API root.
const DefaultGitHubAPI = "https://api.github.com"

// Errors specific to GitHub operations.
var (
	// ErrNotMergeable is returned when GitHub refuses a merge (HTTP 405/409).
	ErrNotMergeable = errors.New("github: pull request is not mergeable")
	// ErrInvalidRepo is returned for repo values that are not "owner/name".
	ErrInvalidRepo = errors.New("github: invalid repository")
	// ErrInvalidPRURL is returned when a pull request URL cannot be parsed.
	ErrInvalidPRURL = errors.New("github: invalid pull request url")
)

var (
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)
)

// ValidateRepo checks that repo has the form "owner/name".
func ValidateRepo(repo string) error {
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("%w: %q (want owner/name)", ErrInvalidRepo, repo)
	}
	return nil
}

// ParsePRURL splits a pull request URL such as
// https://github.com/owner/name/pull/12 into "owner/name" and 12.
func ParsePRURL(raw string) (repo string, number int, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil || u.Host == "" {
		return "", 0, fmt.Errorf("%w: %q", ErrInvalidPRURL, raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", 0, fmt.Errorf("%w: %q", ErrInvalidPRURL, raw)
	}
	n, nerr := strconv.Atoi(parts[3])
	if nerr != nil || n <= 0 {
		return "", 0, fmt.Errorf("%w: %q", ErrInvalidPRURL, raw)
	}
	repo = parts[0] + "/" + parts[1]
	if err := ValidateRepo(repo); err != nil {
		return "", 0, err
	}
	return repo, n, nil
}

// CmdRunner runs an external command and returns its stdout.
type CmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ResolveGitHubToken looks for a token in GH_TOKEN, then GITHUB_TOKEN, then
// the output of `gh auth token`. getenv and run may be nil to use the process
// environment and the real gh binary.
func ResolveGitHubToken(ctx context.Context, getenv func(string) string, run CmdRunner) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if run == nil {
		run = execRunner
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v, nil
		}
	}
	out, err := run(ctx, "gh", "auth", "token")
	if err != nil {
		return "", fmt.Errorf("%w: set GH_TOKEN or GITHUB_TOKEN, or run `gh auth login` (%v)", ErrNoCredentials, err)
	}
	if tok := strings.TrimSpace(string(out)); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("%w: `gh auth token` printed nothing", ErrNoCredentials)
}

// GitHub is a GitHub REST client. It is safe for concurrent use.
type GitHub struct {
	baseURL    *url.URL
	graphqlURL string
	hc         *http.Client
	tokenSrc   func(context.Context) (string, error)

	mu    sync.Mutex
	token string
}

// GitHubOption configures a GitHub client.
type GitHubOption func(*GitHub)

// WithGitHubBaseURL sets the REST API root (for GitHub Enterprise or tests).
// The GraphQL endpoint is derived from it unless overridden.
func WithGitHubBaseURL(u string) GitHubOption {
	return func(g *GitHub) {
		if p, err := url.Parse(strings.TrimRight(u, "/")); err == nil {
			g.baseURL = p
		}
	}
}

// WithGitHubGraphQLURL overrides the GraphQL endpoint.
func WithGitHubGraphQLURL(u string) GitHubOption { return func(g *GitHub) { g.graphqlURL = u } }

// WithGitHubHTTPClient sets the HTTP client.
func WithGitHubHTTPClient(hc *http.Client) GitHubOption { return func(g *GitHub) { g.hc = hc } }

// WithGitHubToken sets a fixed token (an OAuth token or personal access token).
func WithGitHubToken(tok string) GitHubOption {
	return func(g *GitHub) {
		g.tokenSrc = func(context.Context) (string, error) { return tok, nil }
	}
}

// WithGitHubTokenSource sets a custom token lookup.
func WithGitHubTokenSource(f func(context.Context) (string, error)) GitHubOption {
	return func(g *GitHub) { g.tokenSrc = f }
}

// NewGitHub returns a client. Without a token option it resolves credentials
// lazily with ResolveGitHubToken on first use.
func NewGitHub(opts ...GitHubOption) *GitHub {
	base, _ := url.Parse(DefaultGitHubAPI)
	g := &GitHub{baseURL: base, hc: &http.Client{Timeout: 30 * time.Second}}
	g.tokenSrc = func(ctx context.Context) (string, error) { return ResolveGitHubToken(ctx, nil, nil) }
	for _, o := range opts {
		o(g)
	}
	if g.graphqlURL == "" {
		if g.baseURL.Host == "api.github.com" {
			g.graphqlURL = g.baseURL.String() + "/graphql"
		} else {
			// GitHub Enterprise serves REST at /api/v3 and GraphQL at /api/graphql.
			g.graphqlURL = strings.TrimSuffix(g.baseURL.String(), "/v3") + "/graphql"
		}
	}
	return g
}

func (g *GitHub) getToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" {
		return g.token, nil
	}
	tok, err := g.tokenSrc(ctx)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", fmt.Errorf("%w: empty token", ErrNoCredentials)
	}
	g.token = tok
	return tok, nil
}

func (g *GitHub) invalidateToken() {
	g.mu.Lock()
	g.token = ""
	g.mu.Unlock()
}

// sameOrigin reports whether raw points at the API host, so the bearer token
// is never sent elsewhere (for example via a hostile pagination link).
func (g *GitHub) sameOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == g.baseURL.Scheme && u.Host == g.baseURL.Host
}

// do sends an authenticated request to an absolute URL and returns the
// response when its status is one of ok.
func (g *GitHub) do(ctx context.Context, method, rawURL string, body any, ok ...int) (*response, error) {
	if !g.sameOrigin(rawURL) && rawURL != g.graphqlURL {
		return nil, fmt.Errorf("github: refusing request to foreign host %q", rawURL)
	}
	tok, err := g.getToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := newJSONRequest(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "fleet")
	resp, err := send(g.hc, req)
	if err != nil {
		return nil, err
	}
	if len(ok) == 0 {
		ok = []int{http.StatusOK}
	}
	if hasStatus(resp.Status, ok) {
		return resp, nil
	}
	if resp.Status == http.StatusUnauthorized {
		g.invalidateToken()
	}
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(resp.Body, &e)
	msg := e.Message
	for _, x := range e.Errors {
		if x.Message != "" {
			msg += "; " + x.Message
		}
	}
	return nil, &APIError{Service: "github", Method: method, Path: req.URL.Path, Status: resp.Status, Message: msg}
}

func (g *GitHub) endpoint(repo, path string, q url.Values) (string, error) {
	if err := ValidateRepo(repo); err != nil {
		return "", err
	}
	u := *g.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + "/repos/" + repo + path
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// ---------------------------------------------------------------- Issues

// Issue is a GitHub issue (pull requests are excluded from listings).
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	State     string    `json:"state"`
	URL       string    `json:"url"`
	Author    string    `json:"author,omitempty"`
	Labels    []string  `json:"labels,omitempty"`
	Assignees []string  `json:"assignees,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IssueListOptions filters ListIssues.
type IssueListOptions struct {
	State    string   // open (default), closed or all
	Labels   []string // all must match
	Assignee string   // login, "*" for any, "none" for unassigned
	Limit    int      // default 30, maximum 1000
}

type ghIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListIssues lists issues of repo ("owner/name"), following pagination up to
// opts.Limit. Pull requests are filtered out.
func (g *GitHub) ListIssues(ctx context.Context, repo string, opts IssueListOptions) ([]Issue, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 1000)
	state := opts.State
	switch state {
	case "":
		state = "open"
	case "open", "closed", "all":
	default:
		return nil, fmt.Errorf("github: invalid issue state %q", state)
	}
	q := url.Values{"state": {state}, "per_page": {strconv.Itoa(min(limit, 100))}}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}
	if opts.Assignee != "" {
		q.Set("assignee", opts.Assignee)
	}
	next, err := g.endpoint(repo, "/issues", q)
	if err != nil {
		return nil, err
	}
	out := []Issue{}
	for next != "" && len(out) < limit {
		resp, err := g.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		var page []ghIssue
		if err := decode(resp.Body, &page); err != nil {
			return nil, fmt.Errorf("github: list issues: %w", err)
		}
		for _, gi := range page {
			if len(gi.PullRequest) > 0 && string(gi.PullRequest) != "null" {
				continue
			}
			is := Issue{
				Number: gi.Number, Title: gi.Title, Body: gi.Body, State: gi.State, URL: gi.HTMLURL,
				Author: gi.User.Login, CreatedAt: gi.CreatedAt, UpdatedAt: gi.UpdatedAt,
			}
			for _, l := range gi.Labels {
				is.Labels = append(is.Labels, l.Name)
			}
			for _, a := range gi.Assignees {
				is.Assignees = append(is.Assignees, a.Login)
			}
			if out = append(out, is); len(out) == limit {
				break
			}
		}
		next = ""
		if m := linkNext.FindStringSubmatch(resp.Header.Get("Link")); m != nil && g.sameOrigin(m[1]) {
			next = m[1]
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- Pull requests

// PullRequest is the subset of a pull request fleet needs.
type PullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	State   string `json:"state"` // open or closed
	Draft   bool   `json:"draft"`
	Merged  bool   `json:"merged"`
	Head    string `json:"head"` // branch name
	Base    string `json:"base"` // branch name
	HeadSHA string `json:"head_sha"`
	NodeID  string `json:"node_id,omitempty"`

	// Mergeable is nil while GitHub is still computing it.
	Mergeable      *bool  `json:"mergeable,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
}

type ghPull struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	NodeID         string `json:"node_id"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p ghPull) toPR() *PullRequest {
	return &PullRequest{
		Number: p.Number, URL: p.HTMLURL, Title: p.Title, State: p.State, Draft: p.Draft, Merged: p.Merged,
		Head: p.Head.Ref, Base: p.Base.Ref, HeadSHA: p.Head.SHA, NodeID: p.NodeID,
		Mergeable: p.Mergeable, MergeableState: p.MergeableState,
	}
}

// NewPR describes a pull request to create.
type NewPR struct {
	Title string
	Body  string
	Head  string // branch; use "owner:branch" for forks
	Base  string
}

// CreateDraftPR opens a draft pull request. When one already exists for the
// head branch, that pull request is returned instead (idempotent).
func (g *GitHub) CreateDraftPR(ctx context.Context, repo string, in NewPR) (*PullRequest, error) {
	if strings.TrimSpace(in.Title) == "" || in.Head == "" || in.Base == "" {
		return nil, errors.New("github: create pr: title, head and base are required")
	}
	u, err := g.endpoint(repo, "/pulls", nil)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"title": in.Title, "body": in.Body, "head": in.Head, "base": in.Base, "draft": true}
	resp, err := g.do(ctx, http.MethodPost, u, body, http.StatusCreated)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(ae.Message), "already exists") {
			if pr, ferr := g.findOpenPR(ctx, repo, in.Head); ferr == nil && pr != nil {
				return pr, nil
			}
		}
		return nil, err
	}
	var p ghPull
	if err := decode(resp.Body, &p); err != nil {
		return nil, fmt.Errorf("github: create pr: %w", err)
	}
	return p.toPR(), nil
}

func (g *GitHub) findOpenPR(ctx context.Context, repo, head string) (*PullRequest, error) {
	if !strings.Contains(head, ":") {
		head = strings.SplitN(repo, "/", 2)[0] + ":" + head
	}
	u, err := g.endpoint(repo, "/pulls", url.Values{"head": {head}, "state": {"open"}})
	if err != nil {
		return nil, err
	}
	resp, err := g.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var ps []ghPull
	if err := decode(resp.Body, &ps); err != nil || len(ps) == 0 {
		return nil, err
	}
	return ps[0].toPR(), nil
}

// GetPR fetches a pull request.
func (g *GitHub) GetPR(ctx context.Context, repo string, number int) (*PullRequest, error) {
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	u, err := g.endpoint(repo, "/pulls/"+strconv.Itoa(number), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	var p ghPull
	if err := decode(resp.Body, &p); err != nil {
		return nil, fmt.Errorf("github: get pr: %w", err)
	}
	return p.toPR(), nil
}

// MarkPRReady converts a draft pull request to ready for review. It is a
// no-op when the pull request is not a draft. REST has no endpoint for this,
// so the GraphQL mutation is used.
func (g *GitHub) MarkPRReady(ctx context.Context, repo string, number int) error {
	pr, err := g.GetPR(ctx, repo, number)
	if err != nil {
		return err
	}
	if !pr.Draft {
		return nil
	}
	if pr.NodeID == "" {
		return errors.New("github: pull request has no node id")
	}
	body := map[string]any{
		"query":     `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`,
		"variables": map[string]any{"id": pr.NodeID},
	}
	resp, err := g.do(ctx, http.MethodPost, g.graphqlURL, body)
	if err != nil {
		return err
	}
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := decode(resp.Body, &out); err != nil {
		return fmt.Errorf("github: mark ready: %w", err)
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("github: mark pr #%d ready: %s", number, out.Errors[0].Message)
	}
	return nil
}

// MergeOptions configures SquashMerge.
type MergeOptions struct {
	CommitTitle   string
	CommitMessage string
	// SHA, when set, makes the merge fail if the head moved since it was reviewed.
	SHA string
}

// MergeResult is the outcome of a merge.
type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message,omitempty"`
}

// SquashMerge squash-merges a pull request. Refusals (405/409: checks,
// conflicts, moved head) are returned wrapping ErrNotMergeable.
func (g *GitHub) SquashMerge(ctx context.Context, repo string, number int, opts MergeOptions) (*MergeResult, error) {
	if number <= 0 {
		return nil, fmt.Errorf("github: invalid pull request number %d", number)
	}
	u, err := g.endpoint(repo, "/pulls/"+strconv.Itoa(number)+"/merge", nil)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"merge_method": "squash"}
	if opts.CommitTitle != "" {
		body["commit_title"] = opts.CommitTitle
	}
	if opts.CommitMessage != "" {
		body["commit_message"] = opts.CommitMessage
	}
	if opts.SHA != "" {
		body["sha"] = opts.SHA
	}
	resp, err := g.do(ctx, http.MethodPut, u, body)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusMethodNotAllowed || ae.Status == http.StatusConflict) {
			return nil, fmt.Errorf("%w: %w", ErrNotMergeable, err)
		}
		return nil, err
	}
	var res MergeResult
	if err := decode(resp.Body, &res); err != nil {
		return nil, fmt.Errorf("github: merge: %w", err)
	}
	if !res.Merged {
		return &res, fmt.Errorf("%w: %s", ErrNotMergeable, res.Message)
	}
	return &res, nil
}

// ChecksState summarises CI results.
type ChecksState string

const (
	ChecksNone    ChecksState = "none"
	ChecksPending ChecksState = "pending"
	ChecksSuccess ChecksState = "success"
	ChecksFailure ChecksState = "failure"
)

// PRStatus is the merge-readiness of a pull request.
type PRStatus struct {
	PullRequest
	Checks       ChecksState `json:"checks"`
	ChecksTotal  int         `json:"checks_total"`
	ChecksFailed []string    `json:"checks_failed,omitempty"`
	// Ready is true when the pull request is open, not a draft, mergeable,
	// not blocked, and its checks passed (or there are none).
	Ready bool `json:"ready"`
}

// CheckPRStatus reports state, mergeability and CI results of a pull request.
func (g *GitHub) CheckPRStatus(ctx context.Context, repo string, number int) (*PRStatus, error) {
	pr, err := g.GetPR(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	st := &PRStatus{PullRequest: *pr, Checks: ChecksNone}
	if pr.HeadSHA != "" {
		if err := g.collectChecks(ctx, repo, pr.HeadSHA, st); err != nil {
			return nil, err
		}
	}
	st.Ready = pr.State == "open" && !pr.Draft && !pr.Merged &&
		pr.Mergeable != nil && *pr.Mergeable &&
		pr.MergeableState != "dirty" && pr.MergeableState != "blocked" &&
		(st.Checks == ChecksSuccess || st.Checks == ChecksNone)
	return st, nil
}

func (g *GitHub) collectChecks(ctx context.Context, repo, sha string, st *PRStatus) error {
	pending := false

	u, err := g.endpoint(repo, "/commits/"+url.PathEscape(sha)+"/check-runs", url.Values{"per_page": {"100"}})
	if err != nil {
		return err
	}
	resp, err := g.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	var runs struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := decode(resp.Body, &runs); err != nil {
		return fmt.Errorf("github: check runs: %w", err)
	}
	for _, r := range runs.CheckRuns {
		st.ChecksTotal++
		switch {
		case r.Status != "completed":
			pending = true
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
		default:
			st.ChecksFailed = append(st.ChecksFailed, r.Name)
		}
	}

	u, err = g.endpoint(repo, "/commits/"+url.PathEscape(sha)+"/status", nil)
	if err != nil {
		return err
	}
	resp, err = g.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	var comb struct {
		TotalCount int `json:"total_count"`
		Statuses   []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := decode(resp.Body, &comb); err != nil {
		return fmt.Errorf("github: commit status: %w", err)
	}
	for _, s := range comb.Statuses {
		st.ChecksTotal++
		switch s.State {
		case "success":
		case "pending":
			pending = true
		default:
			st.ChecksFailed = append(st.ChecksFailed, s.Context)
		}
	}

	switch {
	case len(st.ChecksFailed) > 0:
		st.Checks = ChecksFailure
	case pending:
		st.Checks = ChecksPending
	case st.ChecksTotal > 0:
		st.Checks = ChecksSuccess
	}
	return nil
}
