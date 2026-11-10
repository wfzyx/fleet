package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/git"
	"fleet/pkg/integrations"
	"fleet/pkg/pi"
	"fleet/pkg/schema"
)

const testRepoSlug = "acme/widgets"

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// fakeWT wraps the real git manager but fakes the network-facing methods.
type fakeWT struct {
	*git.Manager
	mu      sync.Mutex
	pushed  []string
	pushErr error
}

func (f *fakeWT) Push(_ context.Context, _, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, branch)
	return f.pushErr
}

func (f *fakeWT) RemoteRepo(context.Context, string) (string, error) { return testRepoSlug, nil }

type fakeGH struct {
	mu        sync.Mutex
	calls     []string
	created   []integrations.NewPR
	createErr error
	status    *integrations.PRStatus
	mergeErr  error
	merged    []integrations.MergeOptions
}

func (g *fakeGH) record(s string) {
	g.mu.Lock()
	g.calls = append(g.calls, s)
	g.mu.Unlock()
}

func (g *fakeGH) called(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c == name {
			n++
		}
	}
	return n
}

func (g *fakeGH) ListIssues(context.Context, string, integrations.IssueListOptions) ([]integrations.Issue, error) {
	g.record("ListIssues")
	return nil, nil
}

func (g *fakeGH) CreateDraftPR(_ context.Context, repo string, in integrations.NewPR) (*integrations.PullRequest, error) {
	g.record("CreateDraftPR")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.createErr != nil {
		return nil, g.createErr
	}
	g.created = append(g.created, in)
	return &integrations.PullRequest{Number: 5, URL: "https://github.com/" + repo + "/pull/5", Draft: true, Head: in.Head, Base: in.Base}, nil
}

func (g *fakeGH) MarkPRReady(context.Context, string, int) error {
	g.record("MarkPRReady")
	return nil
}

func (g *fakeGH) SquashMerge(_ context.Context, _ string, _ int, o integrations.MergeOptions) (*integrations.MergeResult, error) {
	g.record("SquashMerge")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mergeErr != nil {
		return nil, g.mergeErr
	}
	g.merged = append(g.merged, o)
	return &integrations.MergeResult{SHA: "merged", Merged: true}, nil
}

func (g *fakeGH) CheckPRStatus(context.Context, string, int) (*integrations.PRStatus, error) {
	g.record("CheckPRStatus")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.status == nil {
		return nil, errors.New("fakeGH: no status configured")
	}
	st := *g.status
	return &st, nil
}

type fakeJira struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (j *fakeJira) TransitionIssue(_ context.Context, key, target string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, key+"="+target)
	return j.err
}

func (j *fakeJira) list() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.calls...)
}

// fakeRunner stands in for Pi. By default a worker commits a file in its
// worktree and records some spend; a reviewer approves.
type fakeRunner struct {
	t *testing.T

	mu          sync.Mutex
	workerFn    func(ctx context.Context, o pi.WorkerOptions) (*pi.RunResult, error)
	reviewFn    func(ctx context.Context, o pi.ReviewerOptions) (*agent.ReviewVerdict, error)
	workerCalls []pi.WorkerOptions
	reviewCalls []pi.ReviewerOptions
	workerCost  float64
	reviewCost  float64
}

func (r *fakeRunner) writeSession(o pi.Common, role agent.Role, space, task string, cost float64) {
	if cost <= 0 {
		return
	}
	dir, err := agent.SessionDir(role, space, task)
	if err != nil {
		r.t.Error(err)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Error(err)
		return
	}
	usage := fmt.Sprintf(`{"input":10,"output":10,"cacheRead":0,"cacheWrite":0,"totalTokens":20,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":%g}}`, cost)
	body := fmt.Sprintf(`{"type":"session","id":"sess-%s-%s","timestamp":"2026-01-01T10:00:00.000Z"}`+"\n"+
		`{"type":"message","id":"a1","timestamp":"2026-01-01T10:01:00.000Z","message":{"role":"assistant","provider":"test","model":"m","usage":%s}}`+"\n", role, task, usage)
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(body), 0o644); err != nil {
		r.t.Error(err)
		return
	}
	if o.Tracker != nil {
		if _, err := o.Tracker.IngestDir(space, task, role, dir); err != nil {
			r.t.Error(err)
		}
	}
}

func (r *fakeRunner) RunWorker(ctx context.Context, o pi.WorkerOptions) (*pi.RunResult, error) {
	r.mu.Lock()
	r.workerCalls = append(r.workerCalls, o)
	fn, cost := r.workerFn, r.workerCost
	r.mu.Unlock()
	if o.Common.Tracker != nil {
		if err := o.Common.Tracker.Enforce(o.Space, o.Task); err != nil {
			return nil, err
		}
	}
	if fn != nil {
		return fn(ctx, o)
	}
	file := filepath.Join(o.Worktree, fmt.Sprintf("work-%d.txt", len(r.workerCallsSnapshot())))
	if err := os.WriteFile(file, []byte("change\n"), 0o644); err != nil {
		return nil, err
	}
	gitIn(r.t, o.Worktree, "add", "-A")
	gitIn(r.t, o.Worktree, "commit", "-q", "-m", "work")
	r.writeSession(o.Common, agent.RoleWorker, o.Space, o.Task, cost)
	return &pi.RunResult{Output: "implemented the change", Outcome: agent.OutcomeSucceeded}, nil
}

func (r *fakeRunner) workerCallsSnapshot() []pi.WorkerOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pi.WorkerOptions(nil), r.workerCalls...)
}

func (r *fakeRunner) RunReviewer(ctx context.Context, o pi.ReviewerOptions) (*agent.ReviewVerdict, *pi.RunResult, error) {
	r.mu.Lock()
	r.reviewCalls = append(r.reviewCalls, o)
	fn, cost := r.reviewFn, r.reviewCost
	r.mu.Unlock()
	if fn != nil {
		v, err := fn(ctx, o)
		return v, &pi.RunResult{}, err
	}
	r.writeSession(o.Common, agent.RoleReviewer, o.Space, o.Task, cost)
	v := &agent.ReviewVerdict{Verdict: agent.VerdictApprove, Summary: "looks good"}
	for _, c := range o.Criteria {
		v.Criteria = append(v.Criteria, agent.CriterionResult{Criterion: c, Met: true, Evidence: "file.go:1"})
	}
	return v, &pi.RunResult{}, nil
}

type env struct {
	t      *testing.T
	svc    *Service
	paths  config.Paths
	repo   string
	gh     *fakeGH
	jira   *fakeJira
	runner *fakeRunner
	wt     *fakeWT
}

// newEnv builds a service over temp XDG dirs, a real git repo and fakes for
// Pi, GitHub and Jira. The space "alpha" has one project "app".
func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	rt, err := os.MkdirTemp("", "fl") // short path: unix socket limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rt) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", rt)
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.test",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.test",
	} {
		t.Setenv(k, v)
	}

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")

	paths, err := config.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, paths: paths, repo: repo, gh: &fakeGH{}, jira: &fakeJira{}, runner: &fakeRunner{t: t, workerCost: 0.25, reviewCost: 0.5}}
	e.wt = &fakeWT{Manager: git.NewManager(paths)}
	e.svc = e.open()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.svc.Shutdown(ctx)
	})
	if err := e.svc.AddSpace(config.Space{ID: "alpha", Projects: []config.Project{{ID: "app", Path: repo}}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) open() *Service {
	e.t.Helper()
	svc, err := New(Options{Paths: e.paths, GitHub: e.gh, Jira: e.jira, Worktrees: e.wt, Runner: e.runner})
	if err != nil {
		e.t.Fatal(err)
	}
	return svc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) waitStatus(id string, want schema.Status) TaskRecord {
	e.t.Helper()
	var rec TaskRecord
	waitFor(e.t, fmt.Sprintf("task %s -> %s", id, want), func() bool {
		r, err := e.svc.GetTask(id)
		rec = r
		return err == nil && r.Task.Status == want
	})
	e.waitIdle(id)
	rec, _ = e.svc.GetTask(id)
	return rec
}

func (e *env) waitIdle(id string) {
	e.t.Helper()
	waitFor(e.t, "worker of "+id+" to finish", func() bool {
		e.svc.mu.Lock()
		defer e.svc.mu.Unlock()
		_, running := e.svc.runs[id]
		_, busy := e.svc.busy[id]
		return !running && !busy
	})
}

// toReview delegates, spawns and waits until the worker's report moved the task to review.
func (e *env) toReview(id string) TaskRecord {
	e.t.Helper()
	if _, err := e.svc.Delegate("alpha", id, "Implement "+id, []string{"it works", "tests pass"}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.svc.Spawn(context.Background(), id, ""); err != nil {
		e.t.Fatal(err)
	}
	rec := e.waitStatus(id, schema.StatusReview)
	e.waitIdle(id)
	rec, _ = e.svc.GetTask(id)
	return rec
}

// approve makes the PR look healthy at the reviewed SHA.
func (e *env) approve(id string) string {
	e.t.Helper()
	res, err := e.svc.Review(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	e.gh.mu.Lock()
	e.gh.status = &integrations.PRStatus{
		PullRequest: integrations.PullRequest{Number: 5, State: "open", Draft: true, HeadSHA: res.SHA, Mergeable: ptr(true), MergeableState: "clean"},
		Checks:      integrations.ChecksSuccess,
	}
	e.gh.mu.Unlock()
	return res.SHA
}

func ptr[T any](v T) *T { return &v }

func eventTypes(evs []schema.AgentEvent, taskID string) []schema.EventType {
	var out []schema.EventType
	for _, e := range evs {
		if e.TaskID == taskID {
			out = append(out, e.Type)
		}
	}
	return out
}

func containsStr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
