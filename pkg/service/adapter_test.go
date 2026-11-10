package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/driver"
	"fleet/pkg/schema"
	"fleet/pkg/tui"
)

// adapters returns the same service behind both API implementations.
func adapters(t *testing.T, e *env) map[string]*Adapter {
	t.Helper()
	_, c := startServer(t, e)
	return map[string]*Adapter{"local": NewLocalAdapter(e.svc), "client": NewAdapter(c)}
}

func TestAdapterSpaces(t *testing.T) {
	e := newEnv(t)
	e.toReview("one")
	if _, err := e.svc.Delegate("alpha", "two", "second", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CloseTask(bg, "two", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{MaxUSD: ptr(9.0)}); err != nil {
		t.Fatal(err)
	}
	ls := &leadStarter{}
	e.runner.leadFn = ls.start
	if _, err := e.svc.Chat(bg, "alpha", "hi"); err != nil {
		t.Fatal(err)
	}

	for name, a := range adapters(t, e) {
		got, err := a.Spaces(bg)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: spaces = %+v, %v", name, got, err)
		}
		sp := got[0]
		if sp.ID != "alpha" || sp.Name != "alpha" || sp.BudgetUSD != 9 || sp.ActiveTasks != 1 || sp.Lead != tui.LeadIdle {
			t.Errorf("%s: space = %+v", name, sp)
		}
		if sp.SpendUSD <= 0 {
			t.Errorf("%s: spend = %v, want the worker's cost", name, sp.SpendUSD)
		}
	}
}

func TestAdapterTasksAndDetail(t *testing.T) {
	e := newEnv(t)
	rec := e.toReview("feat")
	e.approve("feat")
	if _, err := e.svc.Review(bg, "feat"); err != nil {
		t.Fatal(err)
	}
	for name, a := range adapters(t, e) {
		ts, err := a.Tasks(bg, "alpha")
		if err != nil || len(ts) != 1 || ts[0].ID != "feat" {
			t.Fatalf("%s: tasks = %+v, %v", name, ts, err)
		}
		if ts, _ = a.Tasks(bg, "other"); len(ts) != 0 {
			t.Errorf("%s: other space tasks = %+v", name, ts)
		}
		d, err := a.Detail(bg, rec.Task)
		if err != nil {
			t.Fatalf("%s: detail: %v", name, err)
		}
		if !strings.Contains(d.Diff, "work-1.txt") {
			t.Errorf("%s: diff = %q", name, d.Diff)
		}
		if len(d.Logs) == 0 || !strings.Contains(strings.Join(d.Logs, "\n"), "worker spawned") {
			t.Errorf("%s: logs = %q", name, d.Logs)
		}
		if d.Verdict == nil || d.Verdict.Verdict != schema.DecisionApproved || len(d.Verdict.Checklist) != 2 {
			t.Errorf("%s: verdict = %+v", name, d.Verdict)
		}
		if _, err := a.Detail(bg, schema.Task{ID: "ghost"}); !errors.Is(err, ErrNoSuchTask) {
			t.Errorf("%s: detail of unknown task: %v", name, err)
		}
	}
}

func TestAdapterCreateTaskAndDelegate(t *testing.T) {
	e := newEnv(t)
	for name, a := range adapters(t, e) {
		title := "Fix the Login Bug! " + name
		t1, err := a.CreateTask(bg, "alpha", title, "details")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantID := "fix-the-login-bug-" + name
		if t1.ID != wantID || t1.Title != title || t1.Status != schema.StatusQueued {
			t.Errorf("%s: created = %+v", name, t1)
		}
		t2, err := a.CreateTask(bg, "alpha", title, "")
		if err != nil || t2.ID != wantID+"-2" || t2.Brief != title {
			t.Errorf("%s: duplicate title: %+v, %v", name, t2, err)
		}
		if _, err := a.CreateTask(bg, "ghost", "x", "y"); !errors.Is(err, ErrNoSuchSpace) {
			t.Errorf("%s: unknown space: %v", name, err)
		}
		if err := a.Delegate(bg, t1); err != nil {
			t.Fatalf("%s: delegate: %v", name, err)
		}
		e.waitStatus(t1.ID, schema.StatusReview) // the fake worker finishes and reports
		if err := a.Delegate(bg, t1); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: delegate of reviewed task: %v", name, err)
		}
	}
}

func TestAdapterRequestReview(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	e.runner.workerFn = func(ctx context.Context, req driver.WorkerRequest) (*driver.WorkerResult, error) {
		gitIn(e.t, req.Worktree, "commit", "-q", "--allow-empty", "-m", "x")
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &driver.WorkerResult{Output: "done", Outcome: agent.OutcomeSucceeded}, nil
	}
	t.Cleanup(func() { close(release) })

	a := NewLocalAdapter(e.svc)
	task, err := a.api.Delegate(bg, DelegateParams{Space: "alpha", Task: "rv", Brief: "b", Criteria: []string{"works"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Delegate(bg, task); err != nil {
		t.Fatal(err)
	}
	// An empty branch has nothing to review: the task still moves to review.
	task.AcceptanceCriteria = []string{"works"}
	if err := a.RequestReview(bg, task); err != nil {
		t.Fatalf("request review: %v", err)
	}
	rec, _ := e.svc.GetTask("rv")
	if rec.Task.Status != schema.StatusReview {
		t.Errorf("status = %s, want review", rec.Task.Status)
	}
	if err := a.RequestReview(bg, task); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("second request: %v", err)
	}
}

func TestAdapterApprove(t *testing.T) {
	e := newEnv(t)
	e.toReview("pr")
	e.approve("pr")
	a := NewLocalAdapter(e.svc)
	rec, _ := e.svc.GetTask("pr")
	if rec.Task.PRUrl == "" {
		t.Fatal("expected a draft PR")
	}
	if err := a.Approve(bg, rec.Task); err != nil {
		t.Fatalf("approve with PR: %v", err)
	}
	if got, _ := e.svc.GetTask("pr"); got.Task.Status != schema.StatusDone || e.gh.called("SquashMerge") != 1 {
		t.Errorf("after approve: %s, merges=%d", got.Task.Status, e.gh.called("SquashMerge"))
	}

	// Without a pull request, approving finishes the task directly.
	e.gh.mu.Lock()
	e.gh.createErr = errors.New("no remote")
	e.gh.mu.Unlock()
	e.toReview("nopr")
	rec, _ = e.svc.GetTask("nopr")
	if rec.Task.PRUrl != "" {
		t.Fatal("unexpected PR")
	}
	if err := a.Approve(bg, rec.Task); err != nil {
		t.Fatalf("approve without PR: %v", err)
	}
	if got, _ := e.svc.GetTask("nopr"); got.Task.Status != schema.StatusDone {
		t.Errorf("status = %s", got.Task.Status)
	}
	if err := a.Approve(bg, schema.Task{ID: "ghost"}); !errors.Is(err, ErrNoSuchTask) {
		t.Errorf("approve unknown: %v", err)
	}
}

func TestAdapterServiceOperations(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start
	for name, a := range adapters(t, e) {
		id := "op-" + name
		if _, err := a.DelegateTask(bg, DelegateParams{Space: "alpha", Task: id, Brief: "b", Criteria: []string{"c"}}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sp, err := a.ListSpaces(bg); err != nil || len(sp) != 1 {
			t.Errorf("%s: spaces %v %v", name, sp, err)
		}
		if _, err := a.Spawn(bg, id, ""); err != nil {
			t.Fatalf("%s: spawn: %v", name, err)
		}
		e.waitStatus(id, schema.StatusReview)
		if _, err := a.Report(bg, id, "progress", "note"); err != nil {
			t.Errorf("%s: report: %v", name, err)
		}
		res, err := a.Review(bg, id)
		if err != nil || res.Verdict.Verdict != schema.DecisionApproved {
			t.Errorf("%s: review = %+v, %v", name, res, err)
		}
		rec, err := a.GetTask(bg, id)
		if err != nil || len(rec.Reviews) != 1 {
			t.Errorf("%s: record = %+v, %v", name, rec, err)
		}
		if ts, err := a.ListTasks(bg, TaskFilter{Status: schema.StatusReview}); err != nil || len(ts) == 0 {
			t.Errorf("%s: list = %v %v", name, ts, err)
		}
		e.approve(id)
		if got, err := a.Merge(bg, id); err != nil || got.Status != schema.StatusDone {
			t.Errorf("%s: merge = %+v, %v", name, got, err)
		}
		if _, err := a.CloseTask(bg, id, "again"); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: close done task: %v", name, err)
		}
		if reply, err := a.SendLeadMessage(bg, "alpha", "ping"); err != nil || reply != "re: ping" {
			t.Errorf("%s: lead = %q, %v", name, reply, err)
		}
		if reply, err := a.Chat(bg, "alpha", "pong"); err != nil || reply != "re: pong" {
			t.Errorf("%s: chat = %q, %v", name, reply, err)
		}
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Fix the Login Bug!":      "fix-the-login-bug",
		"  --weird__ ":            "weird",
		"日本語":                     "task",
		"":                        "task",
		strings.Repeat("ab ", 30): "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-a",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	for in := range cases {
		if err := config.ValidateID(slug(in)); err != nil {
			t.Errorf("slug(%q) is not a valid id: %v", in, err)
		}
	}
}

func TestReviewToSchema(t *testing.T) {
	r := ReviewRecord{
		Verdict: agent.VerdictRequestChanges, Summary: "needs work",
		Criteria: []agent.CriterionResult{{Criterion: "a", Met: true}, {Criterion: " ", Met: false, Evidence: "x.go:2"}},
		Issues:   []agent.Issue{{Severity: agent.SeverityMajor, File: "x.go", Line: 2, Message: "bug"}, {Severity: agent.SeverityNit, Message: "style"}},
	}
	v := reviewToSchema(r)
	if err := v.Validate(); err != nil {
		t.Fatalf("invalid verdict: %v", err)
	}
	if v.Verdict != schema.DecisionChangesRequested || v.Checklist[1].Item != "criterion 2" || v.Checklist[1].Note != "x.go:2" {
		t.Errorf("verdict = %+v", v)
	}
	for _, want := range []string{"needs work", "x.go:2 [major] bug", "[nit] style"} {
		if !strings.Contains(v.Notes, want) {
			t.Errorf("notes %q lack %q", v.Notes, want)
		}
	}
	bare := reviewToSchema(ReviewRecord{Verdict: agent.VerdictReject})
	if err := bare.Validate(); err != nil || bare.Notes != "reject" {
		t.Errorf("bare reject = %+v, %v", bare, err)
	}
	ok := reviewToSchema(ReviewRecord{Verdict: agent.VerdictApprove, Summary: "fine"})
	if ok.Verdict != schema.DecisionApproved || ok.Validate() != nil {
		t.Errorf("approve = %+v", ok)
	}
}
