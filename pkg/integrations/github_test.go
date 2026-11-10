package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testToken = "test-token-not-real"

// ghServer routes requests to handlers keyed by "METHOD /path".
type ghServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	bodies   map[string]string
	auths    []string
}

func newGHServer(t *testing.T, routes map[string]http.HandlerFunc) *ghServer {
	t.Helper()
	s := &ghServer{bodies: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		s.mu.Lock()
		s.requests = append(s.requests, key)
		s.bodies[key] = string(b)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		h, ok := routes[key]
		if !ok {
			http.Error(w, `{"message":"no route `+key+`"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ghServer) client() *GitHub {
	return NewGitHub(WithGitHubBaseURL(s.URL), WithGitHubToken(testToken), WithGitHubHTTPClient(s.Client()))
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestResolveGitHubToken(t *testing.T) {
	ctx := context.Background()
	noGh := func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("gh not logged in")
	}
	ghOut := func(context.Context, string, ...string) ([]byte, error) { return []byte("gh-token\n"), nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	tests := []struct {
		name    string
		env     map[string]string
		run     CmdRunner
		want    string
		wantErr error
	}{
		{"GH_TOKEN first", map[string]string{"GH_TOKEN": "a", "GITHUB_TOKEN": "b"}, ghOut, "a", nil},
		{"GITHUB_TOKEN second", map[string]string{"GITHUB_TOKEN": " b "}, ghOut, "b", nil},
		{"gh auth token last", nil, ghOut, "gh-token", nil},
		{"nothing", nil, noGh, "", ErrNoCredentials},
		{"empty gh output", nil, func(context.Context, string, ...string) ([]byte, error) { return []byte("\n"), nil }, "", ErrNoCredentials},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveGitHubToken(ctx, env(tc.env), tc.run)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestParsePRURL(t *testing.T) {
	repo, n, err := ParsePRURL("https://github.com/acme/widgets/pull/42")
	if err != nil || repo != "acme/widgets" || n != 42 {
		t.Fatalf("got %q %d %v", repo, n, err)
	}
	for _, bad := range []string{"", "not a url", "https://github.com/acme/widgets/issues/4", "https://github.com/acme/widgets/pull/x", "https://github.com/acme/pull/3"} {
		if _, _, err := ParsePRURL(bad); !errors.Is(err, ErrInvalidPRURL) && !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("ParsePRURL(%q) err = %v", bad, err)
		}
	}
}

func TestValidateRepo(t *testing.T) {
	for _, ok := range []string{"a/b", "acme/my-repo.go", "A_b/c_d"} {
		if err := ValidateRepo(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a", "a/b/c", "../x", "a/..", "a b/c", "a/b?x=1"} {
		if err := ValidateRepo(bad); !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestGitHubTokenResolvedLazilyAndSent(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{"GET /repos/o/r/issues": jsonHandler(200, "[]")})
	calls := 0
	g := NewGitHub(WithGitHubBaseURL(srv.URL), WithGitHubHTTPClient(srv.Client()),
		WithGitHubTokenSource(func(context.Context) (string, error) { calls++; return testToken, nil }))
	if calls != 0 {
		t.Fatal("token resolved eagerly")
	}
	for i := 0; i < 2; i++ {
		if _, err := g.ListIssues(context.Background(), "o/r", IssueListOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("token source called %d times, want 1 (cached)", calls)
	}
	if got := srv.auths[0]; got != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got)
	}
}

func TestGitHubNoCredentials(t *testing.T) {
	g := NewGitHub(WithGitHubTokenSource(func(context.Context) (string, error) { return "", ErrNoCredentials }))
	if _, err := g.ListIssues(context.Background(), "o/r", IssueListOptions{}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestGitHubUnauthorizedInvalidatesToken(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues": jsonHandler(401, `{"message":"Bad credentials"}`),
	})
	n := 0
	g := NewGitHub(WithGitHubBaseURL(srv.URL), WithGitHubHTTPClient(srv.Client()),
		WithGitHubTokenSource(func(context.Context) (string, error) { n++; return testToken, nil }))
	for i := 0; i < 2; i++ {
		_, err := g.ListIssues(context.Background(), "o/r", IssueListOptions{})
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatal("error leaks token")
		}
	}
	if n != 2 {
		t.Errorf("token source calls = %d, want 2", n)
	}
}

func TestListIssuesFiltersPRsAndPaginates(t *testing.T) {
	var srvURL string
	page1 := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			jsonHandler(200, `[{"number":3,"title":"third","state":"open","html_url":"u3"}]`)(w, r)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, srvURL))
		jsonHandler(200, `[
			{"number":1,"title":"first","state":"open","html_url":"u1","user":{"login":"ann"},"labels":[{"name":"bug"}],"assignees":[{"login":"bob"}]},
			{"number":2,"title":"a pr","state":"open","html_url":"u2","pull_request":{"url":"x"}}]`)(w, r)
	}
	srv := newGHServer(t, map[string]http.HandlerFunc{"GET /repos/o/r/issues": page1})
	srvURL = srv.URL
	issues, err := srv.client().ListIssues(context.Background(), "o/r", IssueListOptions{Labels: []string{"bug"}, Assignee: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Number != 1 || issues[1].Number != 3 {
		t.Fatalf("issues = %+v", issues)
	}
	if issues[0].Author != "ann" || issues[0].Labels[0] != "bug" || issues[0].Assignees[0] != "bob" {
		t.Errorf("fields not mapped: %+v", issues[0])
	}

	limited, err := srv.client().ListIssues(context.Background(), "o/r", IssueListOptions{Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit: %v %v", limited, err)
	}
}

func TestListIssuesRejectsForeignNextLink(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/issues": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", `<https://evil.example/steal>; rel="next"`)
			jsonHandler(200, `[{"number":1,"title":"x","state":"open"}]`)(w, r)
		},
	})
	issues, err := srv.client().ListIssues(context.Background(), "o/r", IssueListOptions{})
	if err != nil || len(issues) != 1 {
		t.Fatalf("got %v, %v", issues, err)
	}
	if len(srv.requests) != 1 {
		t.Errorf("followed foreign link: %v", srv.requests)
	}
}

func TestListIssuesValidation(t *testing.T) {
	g := NewGitHub(WithGitHubToken(testToken))
	if _, err := g.ListIssues(context.Background(), "bad", IssueListOptions{}); !errors.Is(err, ErrInvalidRepo) {
		t.Errorf("repo: %v", err)
	}
	if _, err := g.ListIssues(context.Background(), "o/r", IssueListOptions{State: "weird"}); err == nil {
		t.Error("expected state error")
	}
}

func TestCreateDraftPR(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/pulls": jsonHandler(201, `{"number":7,"html_url":"https://github.com/o/r/pull/7","title":"T","state":"open","draft":true,"head":{"ref":"feat","sha":"abc"},"base":{"ref":"main"}}`),
	})
	pr, err := srv.client().CreateDraftPR(context.Background(), "o/r", NewPR{Title: "T", Body: "B", Head: "feat", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 || !pr.Draft || pr.Head != "feat" || pr.Base != "main" || pr.HeadSHA != "abc" {
		t.Errorf("pr = %+v", pr)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(srv.bodies["POST /repos/o/r/pulls"]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["draft"] != true || sent["head"] != "feat" || sent["base"] != "main" {
		t.Errorf("request body = %v", sent)
	}
}

func TestCreateDraftPRExistingIsReturned(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/pulls": jsonHandler(422, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for o:feat."}]}`),
		"GET /repos/o/r/pulls": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("head"); got != "o:feat" {
				t.Errorf("head query = %q", got)
			}
			jsonHandler(200, `[{"number":9,"html_url":"u9","state":"open","draft":true,"head":{"ref":"feat"},"base":{"ref":"main"}}]`)(w, r)
		},
	})
	pr, err := srv.client().CreateDraftPR(context.Background(), "o/r", NewPR{Title: "T", Head: "feat", Base: "main"})
	if err != nil || pr.Number != 9 {
		t.Fatalf("pr=%+v err=%v", pr, err)
	}
}

func TestCreateDraftPRValidation(t *testing.T) {
	g := NewGitHub(WithGitHubToken(testToken))
	if _, err := g.CreateDraftPR(context.Background(), "o/r", NewPR{Head: "h", Base: "b"}); err == nil {
		t.Error("expected error for missing title")
	}
}

func TestMarkPRReady(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/7": jsonHandler(200, `{"number":7,"draft":true,"node_id":"PR_node","state":"open"}`),
		"POST /graphql":          jsonHandler(200, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`),
	})
	if err := srv.client().MarkPRReady(context.Background(), "o/r", 7); err != nil {
		t.Fatal(err)
	}
	body := srv.bodies["POST /graphql"]
	if !strings.Contains(body, "markPullRequestReadyForReview") || !strings.Contains(body, "PR_node") {
		t.Errorf("graphql body = %s", body)
	}
}

func TestMarkPRReadyNoopWhenNotDraft(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/7": jsonHandler(200, `{"number":7,"draft":false,"node_id":"n"}`),
	})
	if err := srv.client().MarkPRReady(context.Background(), "o/r", 7); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 1 {
		t.Errorf("requests = %v", srv.requests)
	}
}

func TestMarkPRReadyGraphQLError(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/7": jsonHandler(200, `{"number":7,"draft":true,"node_id":"n"}`),
		"POST /graphql":          jsonHandler(200, `{"errors":[{"message":"Resource not accessible"}]}`),
	})
	err := srv.client().MarkPRReady(context.Background(), "o/r", 7)
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("err = %v", err)
	}
}

func TestSquashMerge(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"PUT /repos/o/r/pulls/7/merge": jsonHandler(200, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`),
	})
	res, err := srv.client().SquashMerge(context.Background(), "o/r", 7, MergeOptions{CommitTitle: "feat: x (#7)", SHA: "abc"})
	if err != nil || !res.Merged || res.SHA != "deadbeef" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var sent map[string]string
	_ = json.Unmarshal([]byte(srv.bodies["PUT /repos/o/r/pulls/7/merge"]), &sent)
	if sent["merge_method"] != "squash" || sent["commit_title"] != "feat: x (#7)" || sent["sha"] != "abc" {
		t.Errorf("body = %v", sent)
	}
}

func TestSquashMergeRefused(t *testing.T) {
	for _, status := range []int{405, 409} {
		srv := newGHServer(t, map[string]http.HandlerFunc{
			"PUT /repos/o/r/pulls/7/merge": jsonHandler(status, `{"message":"Required status check is expected"}`),
		})
		_, err := srv.client().SquashMerge(context.Background(), "o/r", 7, MergeOptions{})
		if !errors.Is(err, ErrNotMergeable) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
	srv := newGHServer(t, map[string]http.HandlerFunc{
		"PUT /repos/o/r/pulls/7/merge": jsonHandler(200, `{"merged":false,"message":"nope"}`),
	})
	if _, err := srv.client().SquashMerge(context.Background(), "o/r", 7, MergeOptions{}); !errors.Is(err, ErrNotMergeable) {
		t.Errorf("merged=false: err = %v", err)
	}
}

func checkRoutes(pull, runs, status string) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/7":                jsonHandler(200, pull),
		"GET /repos/o/r/commits/abc/check-runs": jsonHandler(200, runs),
		"GET /repos/o/r/commits/abc/status":     jsonHandler(200, status),
	}
}

func TestCheckPRStatus(t *testing.T) {
	const readyPull = `{"number":7,"state":"open","draft":false,"mergeable":true,"mergeable_state":"clean","head":{"ref":"f","sha":"abc"},"base":{"ref":"main"}}`
	tests := []struct {
		name       string
		pull, runs string
		status     string
		checks     ChecksState
		ready      bool
		failed     int
	}{
		{"no checks", readyPull, `{"check_runs":[]}`, `{"total_count":0,"statuses":[]}`, ChecksNone, true, 0},
		{"all success", readyPull,
			`{"check_runs":[{"name":"ci","status":"completed","conclusion":"success"},{"name":"lint","status":"completed","conclusion":"skipped"}]}`,
			`{"total_count":1,"statuses":[{"context":"legacy","state":"success"}]}`, ChecksSuccess, true, 0},
		{"pending run", readyPull, `{"check_runs":[{"name":"ci","status":"in_progress"}]}`, `{"statuses":[]}`, ChecksPending, false, 0},
		{"pending status", readyPull, `{"check_runs":[]}`, `{"statuses":[{"context":"x","state":"pending"}]}`, ChecksPending, false, 0},
		{"failed run", readyPull, `{"check_runs":[{"name":"ci","status":"completed","conclusion":"failure"}]}`, `{"statuses":[]}`, ChecksFailure, false, 1},
		{"failed status", readyPull, `{"check_runs":[]}`, `{"statuses":[{"context":"x","state":"error"}]}`, ChecksFailure, false, 1},
		{"draft", `{"number":7,"state":"open","draft":true,"mergeable":true,"mergeable_state":"clean","head":{"sha":"abc"}}`,
			`{"check_runs":[]}`, `{"statuses":[]}`, ChecksNone, false, 0},
		{"conflicts", `{"number":7,"state":"open","mergeable":false,"mergeable_state":"dirty","head":{"sha":"abc"}}`,
			`{"check_runs":[]}`, `{"statuses":[]}`, ChecksNone, false, 0},
		{"mergeable unknown", `{"number":7,"state":"open","mergeable":null,"head":{"sha":"abc"}}`,
			`{"check_runs":[]}`, `{"statuses":[]}`, ChecksNone, false, 0},
		{"blocked", `{"number":7,"state":"open","mergeable":true,"mergeable_state":"blocked","head":{"sha":"abc"}}`,
			`{"check_runs":[]}`, `{"statuses":[]}`, ChecksNone, false, 0},
		{"merged", `{"number":7,"state":"closed","merged":true,"mergeable":null,"head":{"sha":"abc"}}`,
			`{"check_runs":[]}`, `{"statuses":[]}`, ChecksNone, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newGHServer(t, checkRoutes(tc.pull, tc.runs, tc.status))
			st, err := srv.client().CheckPRStatus(context.Background(), "o/r", 7)
			if err != nil {
				t.Fatal(err)
			}
			if st.Checks != tc.checks || st.Ready != tc.ready || len(st.ChecksFailed) != tc.failed {
				t.Errorf("checks=%s ready=%v failed=%v; want %s %v %d", st.Checks, st.Ready, st.ChecksFailed, tc.checks, tc.ready, tc.failed)
			}
		})
	}
}

func TestCheckPRStatusNotFound(t *testing.T) {
	srv := newGHServer(t, nil)
	_, err := srv.client().CheckPRStatus(context.Background(), "o/r", 7)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("not an APIError 404: %v", err)
	}
}

func TestGitHubContextCancelled(t *testing.T) {
	srv := newGHServer(t, map[string]http.HandlerFunc{"GET /repos/o/r/issues": jsonHandler(200, "[]")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := srv.client().ListIssues(ctx, "o/r", IssueListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnterpriseGraphQLURL(t *testing.T) {
	g := NewGitHub(WithGitHubBaseURL("https://ghe.example/api/v3"))
	if g.graphqlURL != "https://ghe.example/api/graphql" {
		t.Errorf("graphql url = %s", g.graphqlURL)
	}
	if g := NewGitHub(); g.graphqlURL != "https://api.github.com/graphql" {
		t.Errorf("default graphql url = %s", g.graphqlURL)
	}
}
