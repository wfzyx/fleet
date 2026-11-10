package integrations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testCreds = JiraCredentials{Site: "example.atlassian.net", Email: "dev@example.test", APIToken: "api-token-not-real"}

func newJiraServer(t *testing.T, routes map[string]http.HandlerFunc) (*ghServer, *Jira) {
	t.Helper()
	srv := newGHServer(t, routes)
	j, err := NewJira(testCreds, WithJiraBaseURL(srv.URL+"/rest/api/3"), WithJiraHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return srv, j
}

func TestParseEnvFile(t *testing.T) {
	got, err := ParseEnvFile("# comment\n\nJIRA_SITE=example.atlassian.net\nexport JIRA_EMAIL = \"a@b.test\"\nJIRA_API_TOKEN='tok=en'\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"JIRA_SITE": "example.atlassian.net", "JIRA_EMAIL": "a@b.test", "JIRA_API_TOKEN": "tok=en"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := ParseEnvFile("garbage line"); err == nil {
		t.Error("expected error for line without =")
	}
}

func TestLoadJiraCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(path, []byte("JIRA_SITE=https://file.atlassian.net/\nJIRA_EMAIL=file@example.test\nJIRA_API_TOKEN=file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"JIRA_EMAIL": "env@example.test"}
	c, err := LoadJiraCredentials(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "env@example.test" || c.APIToken != "file-token" || c.Site != "https://file.atlassian.net/" {
		t.Errorf("creds = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("validate: %v", err)
	}

	_, err = LoadJiraCredentials(filepath.Join(dir, "missing.env"), func(string) string { return "" })
	if !errors.Is(err, ErrNoCredentials) {
		t.Errorf("missing file: err = %v", err)
	}
	// Env only, no file.
	c, err = LoadJiraCredentials(filepath.Join(dir, "missing.env"), func(k string) string {
		return map[string]string{"JIRA_OAUTH_TOKEN": "o", "JIRA_CLOUD_ID": "cid"}[k]
	})
	if err != nil || c.Mode() != JiraAuthOAuth {
		t.Errorf("oauth env: %+v %v", c, err)
	}
	bad := filepath.Join(dir, "bad.env")
	_ = os.WriteFile(bad, []byte("nonsense\n"), 0o600)
	if _, err := LoadJiraCredentials(bad, func(string) string { return "" }); err == nil {
		t.Error("expected parse error")
	}
}

func TestJiraCredentialsPath(t *testing.T) {
	home := func() (string, error) { return "/home/u", nil }
	p, _ := JiraCredentialsPath(func(string) string { return "" }, home)
	if p != "/home/u/.config/jira/credentials.env" {
		t.Errorf("default = %s", p)
	}
	p, _ = JiraCredentialsPath(func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	}, home)
	if p != "/xdg/jira/credentials.env" {
		t.Errorf("xdg = %s", p)
	}
	p, _ = JiraCredentialsPath(func(string) string { return "relative" }, home)
	if p != "/home/u/.config/jira/credentials.env" {
		t.Errorf("relative XDG must be ignored, got %s", p)
	}
}

func TestJiraCredentialsValidate(t *testing.T) {
	tests := []struct {
		name string
		c    JiraCredentials
		ok   bool
	}{
		{"api token", testCreds, true},
		{"missing token", JiraCredentials{Site: "a.b", Email: "e"}, false},
		{"missing email", JiraCredentials{Site: "a.b", APIToken: "t"}, false},
		{"missing site", JiraCredentials{Email: "e", APIToken: "t"}, false},
		{"bad site", JiraCredentials{Site: "a.b/path", Email: "e", APIToken: "t"}, false},
		{"oauth", JiraCredentials{OAuthToken: "o", CloudID: "c"}, true},
		{"oauth without cloud id", JiraCredentials{OAuthToken: "o"}, false},
	}
	for _, tc := range tests {
		if err := tc.c.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	if _, err := NewJira(JiraCredentials{}); !errors.Is(err, ErrInvalidJiraConfig) {
		t.Errorf("NewJira(empty) err = %v", err)
	}
}

func TestJiraAuthHeadersAndBaseURL(t *testing.T) {
	j, err := NewJira(testCreds)
	if err != nil {
		t.Fatal(err)
	}
	if j.baseURL != "https://example.atlassian.net/rest/api/3" || j.AuthMode() != JiraAuthAPIToken {
		t.Errorf("basic: %s %s", j.baseURL, j.AuthMode())
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(testCreds.Email+":"+testCreds.APIToken))
	if j.auth != wantBasic {
		t.Error("basic auth header mismatch")
	}

	o, err := NewJira(JiraCredentials{OAuthToken: "oauth-not-real", CloudID: "cloud-123", Site: "https://example.atlassian.net/"})
	if err != nil {
		t.Fatal(err)
	}
	if o.baseURL != "https://api.atlassian.com/ex/jira/cloud-123/rest/api/3" || o.auth != "Bearer oauth-not-real" || o.AuthMode() != JiraAuthOAuth {
		t.Errorf("oauth: %s %s", o.baseURL, o.auth)
	}
}

const issueJSON = `{"id":"10001","key":"PROJ-1","fields":{
	"summary":"Fix login","status":{"name":"To Do","statusCategory":{"name":"To Do"}},
	"issuetype":{"name":"Bug"},"priority":{"name":"High"},"assignee":{"displayName":"Ann"},
	"project":{"key":"PROJ"},"labels":["a","b"],"updated":"2024-05-01T10:00:00.000+0000",
	"description":{"type":"doc","version":1,"content":[
		{"type":"paragraph","content":[{"type":"text","text":"First line"}]},
		{"type":"paragraph","content":[{"type":"text","text":"Second"},{"type":"hardBreak"},{"type":"text","text":"line"}]}]}}}`

func TestJiraGetIssue(t *testing.T) {
	srv, j := newJiraServer(t, map[string]http.HandlerFunc{"GET /rest/api/3/issue/PROJ-1": jsonHandler(200, issueJSON)})
	is, err := j.GetIssue(context.Background(), "PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if is.Key != "PROJ-1" || is.Summary != "Fix login" || is.Status != "To Do" || is.Type != "Bug" ||
		is.Priority != "High" || is.Assignee != "Ann" || is.Project != "PROJ" || len(is.Labels) != 2 {
		t.Errorf("issue = %+v", is)
	}
	if want := "First line\nSecond\nline"; is.Description != want {
		t.Errorf("description = %q, want %q", is.Description, want)
	}
	if is.URL != "https://example.atlassian.net/browse/PROJ-1" {
		t.Errorf("url = %s", is.URL)
	}
	if is.Updated.IsZero() {
		t.Error("updated not parsed")
	}
	if srv.auths[0] != j.auth {
		t.Error("auth header not sent")
	}
}

func TestJiraGetIssueErrors(t *testing.T) {
	_, j := newJiraServer(t, map[string]http.HandlerFunc{
		"GET /rest/api/3/issue/PROJ-404": jsonHandler(404, `{"errorMessages":["Issue does not exist"],"errors":{}}`),
	})
	_, err := j.GetIssue(context.Background(), "PROJ-404")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "Issue does not exist") {
		t.Errorf("err = %v", err)
	}
	for _, bad := range []string{"", "proj", "PROJ-", "PROJ-1/../x", "-1", "PROJ-1?x"} {
		if _, err := j.GetIssue(context.Background(), bad); !errors.Is(err, ErrInvalidIssueKey) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestJiraStringDescription(t *testing.T) {
	if got := descriptionText(json.RawMessage(`"plain text"`)); got != "plain text" {
		t.Errorf("got %q", got)
	}
	if got := descriptionText(json.RawMessage(`null`)); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestBuildJQL(t *testing.T) {
	tests := []struct {
		name string
		o    JiraListOptions
		want string
		err  bool
	}{
		{"mine", JiraListOptions{AssignedToMe: true, ExcludeDone: true}, `assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC`, false},
		{"project", JiraListOptions{Project: "PROJ", Statuses: []string{"To Do", `A"B`}}, `project = "PROJ" AND status in ("To Do", "A\"B") ORDER BY updated DESC`, false},
		{"assignee id", JiraListOptions{Assignee: "abc123"}, `assignee = "abc123" ORDER BY updated DESC`, false},
		{"raw", JiraListOptions{JQL: "project = X", Project: "IGNORED"}, "project = X", false},
		{"injection", JiraListOptions{Project: `X" OR 1=1`}, "", true},
		{"empty", JiraListOptions{}, "", true},
	}
	for _, tc := range tests {
		got, err := tc.o.BuildJQL()
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("%s: got (%q, %v), want (%q, err=%v)", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestJiraListIssuesPaginates(t *testing.T) {
	var calls int
	_, j := newJiraServer(t, map[string]http.HandlerFunc{
		"POST /rest/api/3/search/jql": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			calls++
			if calls == 1 {
				if body["jql"] != `assignee = currentUser() ORDER BY updated DESC` || body["nextPageToken"] != nil {
					t.Errorf("first body = %v", body)
				}
				jsonHandler(200, `{"issues":[`+issueJSON+`],"nextPageToken":"tok2"}`)(w, r)
				return
			}
			if body["nextPageToken"] != "tok2" {
				t.Errorf("second body = %v", body)
			}
			jsonHandler(200, `{"issues":[`+strings.Replace(issueJSON, "PROJ-1", "PROJ-2", 1)+`]}`)(w, r)
		},
	})
	issues, err := j.ListIssues(context.Background(), JiraListOptions{AssignedToMe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Key != "PROJ-1" || issues[1].Key != "PROJ-2" || calls != 2 {
		t.Errorf("issues=%d calls=%d", len(issues), calls)
	}
	calls = 0
	issues, err = j.ListIssues(context.Background(), JiraListOptions{AssignedToMe: true, Limit: 1})
	if err != nil || len(issues) != 1 || calls != 1 {
		t.Errorf("limit: issues=%d calls=%d err=%v", len(issues), calls, err)
	}
}

const transitionsJSON = `{"transitions":[
	{"id":"11","name":"Start progress","to":{"name":"In Progress"}},
	{"id":"31","name":"Finish","to":{"name":"Done"}}]}`

func TestJiraTransitionIssue(t *testing.T) {
	for _, target := range []string{"In Progress", "in progress", "Finish"} {
		srv, j := newJiraServer(t, map[string]http.HandlerFunc{
			"GET /rest/api/3/issue/PROJ-1/transitions":  jsonHandler(200, transitionsJSON),
			"POST /rest/api/3/issue/PROJ-1/transitions": jsonHandler(204, ""),
		})
		if err := j.TransitionIssue(context.Background(), "PROJ-1", target); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		var sent struct {
			Transition struct{ ID string } `json:"transition"`
		}
		_ = json.Unmarshal([]byte(srv.bodies["POST /rest/api/3/issue/PROJ-1/transitions"]), &sent)
		want := map[string]string{"In Progress": "11", "in progress": "11", "Finish": "31"}[target]
		if sent.Transition.ID != want {
			t.Errorf("%s: transition id = %q, want %q", target, sent.Transition.ID, want)
		}
	}
}

func TestJiraTransitionNoMatch(t *testing.T) {
	srv, j := newJiraServer(t, map[string]http.HandlerFunc{
		"GET /rest/api/3/issue/PROJ-1/transitions": jsonHandler(200, transitionsJSON),
		"GET /rest/api/3/issue/PROJ-1":             jsonHandler(200, issueJSON),
	})
	err := j.TransitionIssue(context.Background(), "PROJ-1", "Blocked")
	if !errors.Is(err, ErrNoTransition) || !strings.Contains(err.Error(), `"Done"`) {
		t.Fatalf("err = %v", err)
	}
	for _, r := range srv.requests {
		if strings.HasPrefix(r, "POST") {
			t.Error("must not POST without a matching transition")
		}
	}
	// Already in the target status: no-op.
	if err := j.TransitionIssue(context.Background(), "PROJ-1", "to do"); err != nil {
		t.Errorf("already in status: %v", err)
	}
	if err := j.TransitionIssue(context.Background(), "PROJ-1", " "); err == nil {
		t.Error("empty target must fail")
	}
}

func TestJiraAPIErrorMessage(t *testing.T) {
	_, j := newJiraServer(t, map[string]http.HandlerFunc{
		"GET /rest/api/3/issue/PROJ-1/transitions":  jsonHandler(200, transitionsJSON),
		"POST /rest/api/3/issue/PROJ-1/transitions": jsonHandler(400, `{"errorMessages":["Field 'x' required"],"errors":{"resolution":"required"}}`),
	})
	err := j.TransitionIssue(context.Background(), "PROJ-1", "Done")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || !strings.Contains(ae.Message, "resolution: required") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testCreds.APIToken) {
		t.Error("error leaks token")
	}
}
