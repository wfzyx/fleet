package integrations

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Errors specific to Jira operations.
var (
	// ErrInvalidIssueKey is returned for keys that are not like "PROJ-123".
	ErrInvalidIssueKey = errors.New("jira: invalid issue key")
	// ErrNoTransition is returned when an issue has no transition to the target status.
	ErrNoTransition = errors.New("jira: no such transition")
	// ErrInvalidJiraConfig is returned for incomplete or malformed credentials.
	ErrInvalidJiraConfig = errors.New("jira: invalid configuration")
)

var (
	issueKeyRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)
	projectKeyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	siteRE       = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]+)?$`)
)

// ValidIssueKey reports whether key looks like a Jira issue key.
func ValidIssueKey(key string) bool { return issueKeyRE.MatchString(key) }

// JiraCredentials holds either API-token credentials (Site, Email, APIToken)
// or OAuth 2.0 credentials (OAuthToken, CloudID).
type JiraCredentials struct {
	Site       string // e.g. example.atlassian.net
	Email      string
	APIToken   string
	OAuthToken string // OAuth 2.0 (3LO) access token
	CloudID    string // Atlassian cloud id, required with OAuthToken
}

// JiraAuthMode says how a client authenticates.
type JiraAuthMode string

const (
	JiraAuthAPIToken JiraAuthMode = "api_token"
	JiraAuthOAuth    JiraAuthMode = "oauth"
)

// Mode returns the authentication mode. OAuth wins when both are present.
func (c JiraCredentials) Mode() JiraAuthMode {
	if c.OAuthToken != "" {
		return JiraAuthOAuth
	}
	return JiraAuthAPIToken
}

// normalizeSite strips a scheme and trailing slash from a site value.
func normalizeSite(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimRight(s, "/")
	if !siteRE.MatchString(s) {
		return "", fmt.Errorf("%w: bad site %q", ErrInvalidJiraConfig, s)
	}
	return s, nil
}

// Validate checks that the credentials are complete for their mode.
func (c JiraCredentials) Validate() error {
	if _, err := normalizeSite(c.Site); err != nil {
		return err
	}
	if c.Mode() == JiraAuthOAuth {
		if c.CloudID == "" {
			return fmt.Errorf("%w: JIRA_CLOUD_ID is required with an OAuth token", ErrInvalidJiraConfig)
		}
		return nil
	}
	switch {
	case c.Site == "":
		return fmt.Errorf("%w: JIRA_SITE is required", ErrInvalidJiraConfig)
	case c.Email == "":
		return fmt.Errorf("%w: JIRA_EMAIL is required", ErrInvalidJiraConfig)
	case c.APIToken == "":
		return fmt.Errorf("%w: JIRA_API_TOKEN is required", ErrInvalidJiraConfig)
	}
	return nil
}

// JiraCredentialsPath returns $XDG_CONFIG_HOME/jira/credentials.env, which
// defaults to ~/.config/jira/credentials.env.
func JiraCredentialsPath(getenv func(string) string, home func() (string, error)) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if home == nil {
		home = os.UserHomeDir
	}
	if v := getenv("XDG_CONFIG_HOME"); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "jira", "credentials.env"), nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("jira: resolve credentials path: %w", err)
	}
	return filepath.Join(h, ".config", "jira", "credentials.env"), nil
}

// ParseEnvFile parses KEY=value lines. Blank lines and #-comments are skipped;
// an optional "export " prefix and one pair of surrounding quotes are removed.
func ParseEnvFile(data string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("line %d: want KEY=value", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

// LoadJiraCredentials reads credentials from the env file at path (a missing
// file is fine), then lets JIRA_* environment variables override it.
// Recognised keys: JIRA_SITE, JIRA_EMAIL, JIRA_API_TOKEN, JIRA_OAUTH_TOKEN,
// JIRA_CLOUD_ID. It returns ErrNoCredentials when nothing was found.
func LoadJiraCredentials(path string, getenv func(string) string) (JiraCredentials, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	vals := map[string]string{}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if vals, err = ParseEnvFile(string(data)); err != nil {
				return JiraCredentials{}, fmt.Errorf("jira: parse %s: %w", path, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return JiraCredentials{}, fmt.Errorf("jira: read %s: %w", path, err)
		}
	}
	get := func(k string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return strings.TrimSpace(vals[k])
	}
	c := JiraCredentials{
		Site: get("JIRA_SITE"), Email: get("JIRA_EMAIL"), APIToken: get("JIRA_API_TOKEN"),
		OAuthToken: get("JIRA_OAUTH_TOKEN"), CloudID: get("JIRA_CLOUD_ID"),
	}
	if c == (JiraCredentials{}) {
		return c, fmt.Errorf("%w: no JIRA_* values in the environment or %s", ErrNoCredentials, path)
	}
	return c, nil
}

// Jira is a Jira Cloud REST v3 client. It is safe for concurrent use.
type Jira struct {
	creds   JiraCredentials
	site    string
	baseURL string // .../rest/api/3
	auth    string // Authorization header value
	hc      *http.Client
}

// JiraOption configures a Jira client.
type JiraOption func(*Jira)

// WithJiraBaseURL overrides the REST root (".../rest/api/3"), for tests.
func WithJiraBaseURL(u string) JiraOption {
	return func(j *Jira) { j.baseURL = strings.TrimRight(u, "/") }
}

// WithJiraHTTPClient sets the HTTP client.
func WithJiraHTTPClient(hc *http.Client) JiraOption { return func(j *Jira) { j.hc = hc } }

// NewJira builds a client from validated credentials.
func NewJira(c JiraCredentials, opts ...JiraOption) (*Jira, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	site, _ := normalizeSite(c.Site)
	j := &Jira{creds: c, site: site, hc: &http.Client{Timeout: 30 * time.Second}}
	if c.Mode() == JiraAuthOAuth {
		j.auth = "Bearer " + c.OAuthToken
		j.baseURL = "https://api.atlassian.com/ex/jira/" + url.PathEscape(c.CloudID) + "/rest/api/3"
	} else {
		j.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Email+":"+c.APIToken))
		j.baseURL = "https://" + site + "/rest/api/3"
	}
	for _, o := range opts {
		o(j)
	}
	return j, nil
}

// NewJiraFromEnv loads credentials from the default credentials file and the
// environment.
func NewJiraFromEnv(opts ...JiraOption) (*Jira, error) {
	path, err := JiraCredentialsPath(nil, nil)
	if err != nil {
		return nil, err
	}
	c, err := LoadJiraCredentials(path, nil)
	if err != nil {
		return nil, err
	}
	return NewJira(c, opts...)
}

// AuthMode returns how the client authenticates.
func (j *Jira) AuthMode() JiraAuthMode { return j.creds.Mode() }

func (j *Jira) do(ctx context.Context, method, path string, q url.Values, body any, ok ...int) (*response, error) {
	u := j.baseURL + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := newJSONRequest(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", j.auth)
	req.Header.Set("User-Agent", "fleet")
	resp, err := send(j.hc, req)
	if err != nil {
		return nil, err
	}
	if len(ok) == 0 {
		ok = []int{http.StatusOK}
	}
	if hasStatus(resp.Status, ok) {
		return resp, nil
	}
	var e struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
		Message       string            `json:"message"`
	}
	_ = json.Unmarshal(resp.Body, &e)
	parts := append([]string{}, e.ErrorMessages...)
	for k, v := range e.Errors {
		parts = append(parts, k+": "+v)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return nil, &APIError{Service: "jira", Method: method, Path: req.URL.Path, Status: resp.Status, Message: strings.Join(parts, "; ")}
}

// JiraIssue is the subset of a Jira issue fleet uses.
type JiraIssue struct {
	Key            string    `json:"key"`
	ID             string    `json:"id"`
	Summary        string    `json:"summary"`
	Description    string    `json:"description,omitempty"`
	Status         string    `json:"status"`
	StatusCategory string    `json:"status_category,omitempty"` // "To Do", "In Progress", "Done"
	Type           string    `json:"type,omitempty"`
	Priority       string    `json:"priority,omitempty"`
	Assignee       string    `json:"assignee,omitempty"`
	Project        string    `json:"project,omitempty"`
	Labels         []string  `json:"labels,omitempty"`
	URL            string    `json:"url,omitempty"`
	Updated        time.Time `json:"updated,omitempty"`
}

var issueFields = []string{"summary", "description", "status", "issuetype", "priority", "assignee", "project", "labels", "updated"}

type rawJiraIssue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Status      *struct {
			Name     string `json:"name"`
			Category struct {
				Name string `json:"name"`
			} `json:"statusCategory"`
		} `json:"status"`
		IssueType *struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority *struct {
			Name string `json:"name"`
		} `json:"priority"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Project *struct {
			Key string `json:"key"`
		} `json:"project"`
		Labels  []string `json:"labels"`
		Updated string   `json:"updated"`
	} `json:"fields"`
}

func (j *Jira) convert(r rawJiraIssue) JiraIssue {
	is := JiraIssue{
		Key: r.Key, ID: r.ID, Summary: r.Fields.Summary,
		Description: descriptionText(r.Fields.Description), Labels: r.Fields.Labels,
	}
	if s := r.Fields.Status; s != nil {
		is.Status, is.StatusCategory = s.Name, s.Category.Name
	}
	if t := r.Fields.IssueType; t != nil {
		is.Type = t.Name
	}
	if p := r.Fields.Priority; p != nil {
		is.Priority = p.Name
	}
	if a := r.Fields.Assignee; a != nil {
		is.Assignee = a.DisplayName
	}
	if p := r.Fields.Project; p != nil {
		is.Project = p.Key
	}
	if j.site != "" {
		is.URL = "https://" + j.site + "/browse/" + r.Key
	}
	// Jira timestamps look like 2024-05-01T10:00:00.000+0000.
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, r.Fields.Updated); err == nil {
			is.Updated = t
			break
		}
	}
	return is
}

// descriptionText flattens a description that is either a plain string (v2)
// or an Atlassian Document Format tree (v3) to text.
func descriptionText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var node adfNode
	if json.Unmarshal(raw, &node) != nil {
		return ""
	}
	var b strings.Builder
	node.text(&b)
	return strings.TrimSpace(b.String())
}

type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
}

func (n adfNode) text(b *strings.Builder) {
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
	case "hardBreak":
		b.WriteByte('\n')
	case "mention", "emoji":
		b.WriteString(n.Text)
	}
	for _, c := range n.Content {
		c.text(b)
	}
	switch n.Type {
	case "paragraph", "heading", "listItem", "codeBlock", "blockquote", "rule":
		b.WriteByte('\n')
	}
}

// JiraListOptions filters ListIssues.
type JiraListOptions struct {
	// JQL, when set, is used verbatim and all other filters are ignored.
	JQL string
	// Project restricts to a project key.
	Project string
	// AssignedToMe restricts to the authenticated user.
	AssignedToMe bool
	// Assignee restricts to an account id (ignored when AssignedToMe).
	Assignee string
	// Statuses restricts to the named statuses.
	Statuses []string
	// ExcludeDone drops issues whose status category is Done.
	ExcludeDone bool
	// Limit caps the results; default 50, maximum 1000.
	Limit int
}

func jqlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// BuildJQL renders the filters as a JQL query.
func (o JiraListOptions) BuildJQL() (string, error) {
	if strings.TrimSpace(o.JQL) != "" {
		return o.JQL, nil
	}
	var clauses []string
	if o.Project != "" {
		if !projectKeyRE.MatchString(o.Project) {
			return "", fmt.Errorf("jira: invalid project key %q", o.Project)
		}
		clauses = append(clauses, "project = "+jqlQuote(o.Project))
	}
	switch {
	case o.AssignedToMe:
		clauses = append(clauses, "assignee = currentUser()")
	case o.Assignee != "":
		clauses = append(clauses, "assignee = "+jqlQuote(o.Assignee))
	}
	if len(o.Statuses) > 0 {
		qs := make([]string, len(o.Statuses))
		for i, s := range o.Statuses {
			qs[i] = jqlQuote(s)
		}
		clauses = append(clauses, "status in ("+strings.Join(qs, ", ")+")")
	}
	if o.ExcludeDone {
		clauses = append(clauses, "statusCategory != Done")
	}
	if len(clauses) == 0 {
		// The search API rejects unbounded queries.
		return "", errors.New("jira: list issues needs a project, assignee, status or JQL filter")
	}
	return strings.Join(clauses, " AND ") + " ORDER BY updated DESC", nil
}

// ListIssues searches issues, following pagination up to opts.Limit.
func (j *Jira) ListIssues(ctx context.Context, opts JiraListOptions) ([]JiraIssue, error) {
	jql, err := opts.BuildJQL()
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 1000)
	out := []JiraIssue{}
	token := ""
	for len(out) < limit {
		body := map[string]any{"jql": jql, "fields": issueFields, "maxResults": min(limit-len(out), 100)}
		if token != "" {
			body["nextPageToken"] = token
		}
		resp, err := j.do(ctx, http.MethodPost, "/search/jql", nil, body)
		if err != nil {
			return nil, err
		}
		var page struct {
			Issues        []rawJiraIssue `json:"issues"`
			NextPageToken string         `json:"nextPageToken"`
		}
		if err := decode(resp.Body, &page); err != nil {
			return nil, fmt.Errorf("jira: search: %w", err)
		}
		for _, r := range page.Issues {
			out = append(out, j.convert(r))
		}
		if page.NextPageToken == "" || len(page.Issues) == 0 {
			break
		}
		token = page.NextPageToken
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetIssue fetches one issue by key.
func (j *Jira) GetIssue(ctx context.Context, key string) (*JiraIssue, error) {
	if !ValidIssueKey(key) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidIssueKey, key)
	}
	q := url.Values{"fields": {strings.Join(issueFields, ",")}}
	resp, err := j.do(ctx, http.MethodGet, "/issue/"+url.PathEscape(key), q, nil)
	if err != nil {
		return nil, err
	}
	var r rawJiraIssue
	if err := decode(resp.Body, &r); err != nil {
		return nil, fmt.Errorf("jira: get issue: %w", err)
	}
	is := j.convert(r)
	return &is, nil
}

// JiraTransition is a workflow transition available on an issue.
type JiraTransition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   string `json:"to"` // target status name
}

// Transitions lists the transitions currently available on an issue.
func (j *Jira) Transitions(ctx context.Context, key string) ([]JiraTransition, error) {
	if !ValidIssueKey(key) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidIssueKey, key)
	}
	resp, err := j.do(ctx, http.MethodGet, "/issue/"+url.PathEscape(key)+"/transitions", nil, nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := decode(resp.Body, &r); err != nil {
		return nil, fmt.Errorf("jira: transitions: %w", err)
	}
	out := make([]JiraTransition, len(r.Transitions))
	for i, t := range r.Transitions {
		out[i] = JiraTransition{ID: t.ID, Name: t.Name, To: t.To.Name}
	}
	return out, nil
}

// TransitionIssue moves an issue to the status (or transition) named target,
// for example "In Progress" or "Done", compared case-insensitively. It is a
// no-op when the issue is already in that status. When no transition
// matches, the error wraps ErrNoTransition and lists the available ones.
func (j *Jira) TransitionIssue(ctx context.Context, key, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("jira: transition target is required")
	}
	ts, err := j.Transitions(ctx, key)
	if err != nil {
		return err
	}
	var match *JiraTransition
	// Prefer a transition whose destination status matches over one that only
	// shares the transition name.
	for i := range ts {
		if strings.EqualFold(ts[i].To, target) {
			match = &ts[i]
			break
		}
	}
	if match == nil {
		for i := range ts {
			if strings.EqualFold(ts[i].Name, target) {
				match = &ts[i]
				break
			}
		}
	}
	if match == nil {
		if is, gerr := j.GetIssue(ctx, key); gerr == nil && strings.EqualFold(is.Status, target) {
			return nil
		}
		names := make([]string, len(ts))
		for i, t := range ts {
			names[i] = strconv.Quote(t.To)
		}
		return fmt.Errorf("%w: %s -> %q (available: %s)", ErrNoTransition, key, target, strings.Join(names, ", "))
	}
	body := map[string]any{"transition": map[string]string{"id": match.ID}}
	_, err = j.do(ctx, http.MethodPost, "/issue/"+url.PathEscape(key)+"/transitions", nil, body, http.StatusNoContent, http.StatusOK)
	return err
}
