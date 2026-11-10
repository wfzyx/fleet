package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/integrations"
	"fleet/pkg/pi"
	"fleet/pkg/schema"
)

var bg = context.Background()

func TestSpacesLifecycle(t *testing.T) {
	e := newEnv(t)
	svc := e.svc

	if err := svc.AddSpace(config.Space{ID: "alpha"}); !errors.Is(err, ErrSpaceExists) {
		t.Errorf("duplicate: %v", err)
	}
	if err := svc.AddSpace(config.Space{ID: "../evil"}); err == nil {
		t.Error("unsafe id accepted")
	}
	if err := svc.AddSpace(config.Space{ID: "gone", Projects: []config.Project{{ID: "p", Path: "/definitely/not/here"}}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("missing project dir: %v", err)
	}
	if err := svc.AddSpace(config.Space{ID: "rel", Projects: []config.Project{{ID: "p", Path: "relative"}}}); err == nil {
		t.Error("relative path accepted")
	}
	if err := svc.AddSpace(config.Space{ID: "beta", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	if got := svc.ListSpaces(); len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "beta" || got[0].Name != "alpha" {
		t.Errorf("spaces = %+v", got)
	}
	if err := svc.AddProject("beta", config.Project{ID: "lib", Path: e.repo}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddProject("beta", config.Project{ID: "lib", Path: e.repo}); err == nil {
		t.Error("duplicate project accepted")
	}
	if err := svc.AddProject("nope", config.Project{ID: "x", Path: e.repo}); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("project in missing space: %v", err)
	}
	if _, err := svc.GetSpace("nope"); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("GetSpace: %v", err)
	}

	// Returned values must be copies.
	sp, _ := svc.GetSpace("beta")
	sp.Projects[0].Path = "/mutated"
	if again, _ := svc.GetSpace("beta"); again.Projects[0].Path != e.repo {
		t.Error("GetSpace leaks internal state")
	}

	// Persisted across a restart.
	svc2 := e.open()
	defer svc2.Shutdown(bg)
	if got := svc2.ListSpaces(); len(got) != 2 || len(got[1].Projects) != 1 {
		t.Errorf("after reload: %+v", got)
	}

	if err := svc.RemoveSpace(bg, "beta", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveSpace(bg, "beta", false); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("second remove: %v", err)
	}
	cfg, err := config.Load(e.paths.ConfigFile())
	if err != nil || len(cfg.Spaces) != 1 {
		t.Errorf("config after remove: %+v %v", cfg, err)
	}
}

func TestRemoveSpaceWithActiveTasks(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Delegate("alpha", "t1", "do it", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveSpace(bg, "alpha", false); !errors.Is(err, ErrSpaceInUse) {
		t.Fatalf("err = %v", err)
	}
	if err := e.svc.RemoveSpace(bg, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.ListTasks(TaskFilter{}); len(got) != 0 {
		t.Errorf("tasks left: %+v", got)
	}
	if _, err := e.svc.GetTask("t1"); !errors.Is(err, ErrNoSuchTask) {
		t.Errorf("err = %v", err)
	}
}

func TestBudgetUpdates(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{MaxUSD: ptr(5.0), TaskMaxUSD: ptr(1.5)}); err != nil {
		t.Fatal(err)
	}
	sp, _ := e.svc.GetSpace("alpha")
	if sp.MaxBudgetUSD != 5 || sp.TaskBudgetUSD != 1.5 {
		t.Errorf("space = %+v", sp)
	}
	// nil leaves values alone
	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{MaxUSD: ptr(7.0)}); err != nil {
		t.Fatal(err)
	}
	if sp, _ = e.svc.GetSpace("alpha"); sp.MaxBudgetUSD != 7 || sp.TaskBudgetUSD != 1.5 {
		t.Errorf("space = %+v", sp)
	}
	for _, bad := range []float64{-1, nan(), inf()} {
		if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{MaxUSD: &bad}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("budget %v: %v", bad, err)
		}
		if err := e.svc.SetFleetBudget(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("fleet budget %v: %v", bad, err)
		}
	}
	if err := e.svc.SetSpaceBudget("nope", BudgetUpdate{MaxUSD: ptr(1.0)}); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("err = %v", err)
	}
	if err := e.svc.SetFleetBudget(100); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(e.paths.ConfigFile())
	if err != nil || cfg.Budget.MaxBudgetUSD != 100 || cfg.Spaces[0].MaxBudgetUSD != 7 {
		t.Errorf("persisted config = %+v, %v", cfg, err)
	}
}

func nan() float64 { z := 0.0; return z / z }
func inf() float64 { z := 0.0; return 1 / z }

func TestDelegate(t *testing.T) {
	e := newEnv(t)
	task, err := e.svc.Delegate("alpha", "fix-1", "  Fix the bug\nwith details  ", []string{" a ", "", "b"}, WithJiraKey("PROJ-12"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != schema.StatusQueued || task.Title != "Fix the bug" || task.ProjectID != "app" ||
		len(task.AcceptanceCriteria) != 2 || task.AcceptanceCriteria[0] != "a" || task.JiraKey != "PROJ-12" {
		t.Errorf("task = %+v", task)
	}

	cases := []struct {
		name             string
		space, id, brief string
		opts             []DelegateOption
		want             error
	}{
		{"duplicate", "alpha", "fix-1", "x", nil, ErrTaskExists},
		{"no space", "nope", "t2", "x", nil, ErrNoSuchSpace},
		{"empty brief", "alpha", "t2", "  ", nil, ErrInvalidArgument},
		{"bad id", "alpha", "../x", "x", nil, ErrInvalidArgument},
		{"dotdot id", "alpha", "a..b", "x", nil, ErrInvalidArgument},
		{"bad jira", "alpha", "t2", "x", []DelegateOption{WithJiraKey("nope")}, integrations.ErrInvalidIssueKey},
		{"bad project", "alpha", "t2", "x", []DelegateOption{WithProject("zzz")}, ErrNoProject},
	}
	for _, tc := range cases {
		if _, err := e.svc.Delegate(tc.space, tc.id, tc.brief, nil, tc.opts...); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	// Several projects require an explicit choice.
	if err := e.svc.AddProject("alpha", config.Project{ID: "lib", Path: e.repo}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Delegate("alpha", "t3", "x", nil); !errors.Is(err, ErrNoProject) {
		t.Errorf("ambiguous project: %v", err)
	}
	if got, err := e.svc.Delegate("alpha", "t3", "x", nil, WithProject("lib"), WithTitle("Custom")); err != nil || got.ProjectID != "lib" || got.Title != "Custom" {
		t.Errorf("explicit project: %+v %v", got, err)
	}

	if got := e.svc.ListTasks(TaskFilter{Space: "alpha", Status: schema.StatusQueued}); len(got) != 2 || got[0].ID != "fix-1" {
		t.Errorf("list = %+v", got)
	}
	if got := e.svc.ListTasks(TaskFilter{Status: schema.StatusDone}); len(got) != 0 {
		t.Errorf("done list = %+v", got)
	}
}

func TestFullLifecycle(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Delegate("alpha", "feat-1", "Add the feature", []string{"it works", "tests pass"}, WithJiraKey("PROJ-7")); err != nil {
		t.Fatal(err)
	}

	task, err := e.svc.Spawn(bg, "feat-1", "")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(e.paths.DataDir, "worktrees", "alpha", "feat-1")
	if task.Status != schema.StatusWorking || task.WorktreePath != wantPath || task.Branch != "fleet/feat-1" || task.AssignedWorker == "" {
		t.Fatalf("spawned task = %+v", task)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "README.md")); err != nil {
		t.Errorf("worktree not checked out: %v", err)
	}

	// The worker finishes by itself: auto-report -> review, branch pushed, draft PR opened.
	rec := e.waitStatus("feat-1", schema.StatusReview)
	if rec.Task.PRUrl != "https://github.com/acme/widgets/pull/5" || rec.GitHubRepo != testRepoSlug {
		t.Errorf("PR not recorded: %+v", rec.Task)
	}
	if len(e.wt.pushed) != 1 || e.wt.pushed[0] != "fleet/feat-1" {
		t.Errorf("pushed = %v", e.wt.pushed)
	}
	if len(e.gh.created) != 1 || e.gh.created[0].Base != "main" || e.gh.created[0].Head != "fleet/feat-1" ||
		!strings.Contains(e.gh.created[0].Body, "tests pass") {
		t.Errorf("created PRs = %+v", e.gh.created)
	}
	call := e.runner.workerCallsSnapshot()[0]
	if call.Worktree != wantPath || call.Branch != "fleet/feat-1" || len(call.Acceptance) != 2 || call.Common.Tracker == nil {
		t.Errorf("worker options = %+v", call)
	}

	// Review: reviewer receives a unified diff and the criteria.
	approved, err := e.svc.Review(bg, "feat-1")
	if err != nil {
		t.Fatal(err)
	}
	if approved.Verdict.Verdict != schema.DecisionApproved || len(approved.Verdict.Checklist) != 2 || approved.SHA == "" {
		t.Errorf("review result = %+v", approved)
	}
	rc := e.runner.reviewCalls[0]
	if !strings.Contains(rc.Diff, "+change") || !strings.Contains(rc.Diff, "work-1.txt") || len(rc.Criteria) != 2 {
		t.Errorf("reviewer options = %+v", rc)
	}
	if rec, _ = e.svc.GetTask("feat-1"); rec.Task.Status != schema.StatusReview || len(rec.Reviews) != 1 {
		t.Fatalf("after approval: %+v", rec)
	}

	// Merge.
	e.gh.status = &integrations.PRStatus{
		PullRequest: integrations.PullRequest{Number: 5, State: "open", Draft: true, HeadSHA: approved.SHA, Mergeable: ptr(true)},
		Checks:      integrations.ChecksSuccess,
	}
	done, err := e.svc.Merge(bg, "feat-1")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != schema.StatusDone || done.WorktreePath != "" {
		t.Errorf("merged task = %+v", done)
	}
	if e.gh.called("MarkPRReady") != 1 || e.gh.called("SquashMerge") != 1 {
		t.Errorf("gh calls = %v", e.gh.calls)
	}
	if m := e.gh.merged[0]; m.SHA != approved.SHA || m.CommitTitle != "Add the feature (#5)" {
		t.Errorf("merge options = %+v", m)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Errorf("worktree not removed: %v", err)
	}
	if out := gitIn(t, e.repo, "branch", "--list", "fleet/feat-1"); strings.TrimSpace(out) != "" {
		t.Errorf("branch not deleted: %q", out)
	}
	if got := e.jira.list(); len(got) != 2 || got[0] != "PROJ-7=In Progress" || got[1] != "PROJ-7=Done" {
		t.Errorf("jira calls = %v", got)
	}

	// Spend: worker 0.25 + reviewer 0.5, on the task and fleet totals; sessions cleaned.
	rec, _ = e.svc.GetTask("feat-1")
	if rec.Task.SpendUSD < 0.7499 || rec.Task.SpendUSD > 0.7501 {
		t.Errorf("task spend = %v", rec.Task.SpendUSD)
	}
	cfg, _ := config.Load(e.paths.ConfigFile())
	if cfg.Budget.CurrentSpendUSD < 0.7499 || cfg.Budget.CurrentSpendUSD > 0.7501 {
		t.Errorf("fleet spend = %v", cfg.Budget.CurrentSpendUSD)
	}
	if dir, _ := agent.SessionDir(agent.RoleWorker, "alpha", "feat-1"); dirExists(dir) {
		t.Error("worker session not cleaned up")
	}

	// Events: strict, in order, persisted.
	evs, err := e.svc.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	want := []schema.EventType{
		schema.EventTaskProgress, schema.EventTaskStarted, schema.EventTaskProgress,
		schema.EventTaskReviewRequested, schema.EventReviewVerdict, schema.EventTaskCompleted,
	}
	got := eventTypes(evs, "feat-1")
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
	for _, ev := range evs {
		if err := ev.Validate(); err != nil {
			t.Errorf("invalid event logged: %v", err)
		}
	}
	if last := evs[len(evs)-1]; last.PRUrl == "" || last.Type != schema.EventTaskCompleted {
		t.Errorf("last event = %+v", last)
	}
	if tail, _ := e.svc.Events(2); len(tail) != 2 || tail[1].Type != schema.EventTaskCompleted {
		t.Errorf("Events(2) = %+v", tail)
	}

	// The log records every step.
	if len(rec.Log) < 4 || rec.Log[0].Note != "delegated" {
		t.Errorf("log = %+v", rec.Log)
	}
}

func dirExists(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

func TestSpawnGuards(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Spawn(bg, "nope", ""); !errors.Is(err, ErrNoSuchTask) {
		t.Errorf("missing: %v", err)
	}
	release := make(chan struct{})
	e.runner.workerFn = func(ctx context.Context, _ pi.WorkerOptions) (*pi.RunResult, error) {
		select {
		case <-release:
			return &pi.RunResult{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t1", "bad branch name"); err == nil {
		t.Error("invalid branch accepted")
	}
	if rec, _ := e.svc.GetTask("t1"); rec.Task.Status != schema.StatusQueued || rec.Task.WorktreePath != "" {
		t.Errorf("failed spawn changed state: %+v", rec.Task)
	}
	if _, err := e.svc.Spawn(bg, "t1", "feature/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t1", ""); !errors.Is(err, ErrBusy) {
		t.Errorf("double spawn: %v", err)
	}
	close(release)
	e.waitStatus("t1", schema.StatusReview)
	if _, err := e.svc.Spawn(bg, "t1", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("spawn in review: %v", err)
	}
}

func TestWorkerFailureRequeuesAndRespawnReusesWorktree(t *testing.T) {
	e := newEnv(t)
	fail := true
	e.runner.workerFn = func(_ context.Context, o pi.WorkerOptions) (*pi.RunResult, error) {
		if fail {
			return &pi.RunResult{}, &pi.ExitError{Code: 2, Stderr: "boom"}
		}
		return &pi.RunResult{Output: "ok"}, nil
	}
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	first, err := e.svc.Spawn(bg, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	rec := e.waitStatus("t1", schema.StatusQueued)
	e.waitIdle("t1")
	if !strings.Contains(rec.Log[len(rec.Log)-1].Note, "boom") {
		t.Errorf("failure not logged: %+v", rec.Log)
	}
	if rec.Task.WorktreePath != first.WorktreePath || !dirExists(first.WorktreePath) {
		t.Errorf("worktree should be kept for inspection: %+v", rec.Task)
	}

	fail = false
	second, err := e.svc.Spawn(bg, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.WorktreePath != first.WorktreePath || second.Branch != first.Branch {
		t.Errorf("respawn changed worktree: %+v", second)
	}
	e.waitStatus("t1", schema.StatusReview)
}

func TestReviewChangesRequestedSendsTaskBackToWork(t *testing.T) {
	e := newEnv(t)
	e.toReview("t1")
	e.runner.reviewFn = func(_ context.Context, o pi.ReviewerOptions) (*agent.ReviewVerdict, error) {
		return &agent.ReviewVerdict{
			Verdict: agent.VerdictRequestChanges, Summary: "missing tests",
			Criteria: []agent.CriterionResult{{Criterion: "it works", Met: true}, {Criterion: "tests pass", Met: false, Evidence: "none"}},
			Issues:   []agent.Issue{{Severity: agent.SeverityMajor, File: "a.go", Line: 3, Message: "add a test"}},
		}, nil
	}
	res, err := e.svc.Review(bg, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict.Verdict != schema.DecisionChangesRequested || !strings.Contains(res.Verdict.Notes, "a.go:3: add a test") {
		t.Errorf("verdict = %+v", res.Verdict)
	}
	rec := e.waitStatus("t1", schema.StatusWorking)
	if !strings.Contains(rec.Feedback, "missing tests") || len(rec.Reviews) != 1 {
		t.Errorf("record = %+v", rec)
	}
	// Merge must be refused now, and respawn carries the feedback to the worker.
	if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("merge while working: %v", err)
	}
	if _, err := e.svc.Spawn(bg, "t1", ""); err != nil {
		t.Fatal(err)
	}
	e.waitStatus("t1", schema.StatusReview)
	calls := e.runner.workerCallsSnapshot()
	if len(calls) != 2 || !strings.Contains(calls[1].Brief, "Reviewer feedback") || !strings.Contains(calls[1].Brief, "add a test") {
		t.Errorf("second worker brief = %q", calls[len(calls)-1].Brief)
	}
	if strings.Contains(calls[0].Brief, "Reviewer feedback") {
		t.Error("first brief must not contain feedback")
	}
	// The PR already exists: no second draft is opened, but the branch is pushed again.
	if e.gh.called("CreateDraftPR") != 1 || len(e.wt.pushed) != 2 {
		t.Errorf("CreateDraftPR=%d pushed=%v", e.gh.called("CreateDraftPR"), e.wt.pushed)
	}
}

func TestReviewGuards(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Review(bg, "nope"); !errors.Is(err, ErrNoSuchTask) {
		t.Errorf("missing: %v", err)
	}
	if _, err := e.svc.Delegate("alpha", "t0", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Review(bg, "t0"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("queued: %v", err)
	}

	e.toReview("t1")
	// reviewer failure leaves the task in review and is reported
	e.runner.reviewFn = func(context.Context, pi.ReviewerOptions) (*agent.ReviewVerdict, error) {
		return nil, agent.ErrNoVerdict
	}
	if _, err := e.svc.Review(bg, "t1"); !errors.Is(err, agent.ErrNoVerdict) {
		t.Errorf("reviewer error: %v", err)
	}
	if rec, _ := e.svc.GetTask("t1"); rec.Task.Status != schema.StatusReview || len(rec.Reviews) != 0 {
		t.Errorf("state after failed review: %+v", rec.Task)
	}

	// no criteria
	e.runner.reviewFn = nil
	if _, err := e.svc.Delegate("alpha", "nocrit", "x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "nocrit", ""); err != nil {
		t.Fatal(err)
	}
	e.waitStatus("nocrit", schema.StatusReview)
	if _, err := e.svc.Review(bg, "nocrit"); !errors.Is(err, ErrNoCriteria) {
		t.Errorf("no criteria: %v", err)
	}
}

func TestReviewEmptyDiff(t *testing.T) {
	e := newEnv(t)
	e.runner.workerFn = func(context.Context, pi.WorkerOptions) (*pi.RunResult, error) {
		return &pi.RunResult{Output: "nothing to do"}, nil
	}
	e.toReview("t1")
	if _, err := e.svc.Review(bg, "t1"); !errors.Is(err, ErrEmptyDiff) {
		t.Fatalf("err = %v", err)
	}
}

func TestMergeGuards(t *testing.T) {
	e := newEnv(t)
	e.toReview("t1")

	if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, ErrNotApproved) {
		t.Errorf("unreviewed: %v", err)
	}
	sha := e.approve("t1")

	setStatus := func(mod func(*integrations.PRStatus)) {
		e.gh.mu.Lock()
		defer e.gh.mu.Unlock()
		st := &integrations.PRStatus{
			PullRequest: integrations.PullRequest{Number: 5, State: "open", HeadSHA: sha, Mergeable: ptr(true)},
			Checks:      integrations.ChecksSuccess,
		}
		mod(st)
		e.gh.status = st
	}
	tests := []struct {
		name string
		mod  func(*integrations.PRStatus)
		want error
	}{
		{"checks failing", func(s *integrations.PRStatus) { s.Checks = integrations.ChecksFailure; s.ChecksFailed = []string{"ci"} }, ErrChecksFailing},
		{"checks pending", func(s *integrations.PRStatus) { s.Checks = integrations.ChecksPending }, ErrChecksPending},
		{"conflicts", func(s *integrations.PRStatus) { s.Mergeable = ptr(false) }, ErrConflicts},
		{"closed", func(s *integrations.PRStatus) { s.State = "closed" }, ErrPRClosed},
		{"stale review", func(s *integrations.PRStatus) { s.HeadSHA = "0123456789abcdef" }, ErrStaleReview},
	}
	for _, tc := range tests {
		setStatus(tc.mod)
		if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if e.gh.called("SquashMerge") != 0 || e.gh.called("MarkPRReady") != 0 {
		t.Errorf("guarded merges reached GitHub: %v", e.gh.calls)
	}
	if rec, _ := e.svc.GetTask("t1"); rec.Task.Status != schema.StatusReview || rec.Task.WorktreePath == "" {
		t.Errorf("guarded merge changed state: %+v", rec.Task)
	}

	// GitHub refuses the merge itself: task stays in review.
	setStatus(func(*integrations.PRStatus) {})
	e.gh.mergeErr = integrations.ErrNotMergeable
	if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, integrations.ErrNotMergeable) {
		t.Errorf("merge refused: %v", err)
	}
	if rec, _ := e.svc.GetTask("t1"); rec.Task.Status != schema.StatusReview {
		t.Errorf("status = %s", rec.Task.Status)
	}
	e.gh.mergeErr = nil
	mergeCalls := e.gh.called("SquashMerge")

	// An already merged PR just completes the task, without another merge call.
	setStatus(func(s *integrations.PRStatus) { s.Merged = true; s.State = "closed"; s.HeadSHA = "other" })
	done, err := e.svc.Merge(bg, "t1")
	if err != nil || done.Status != schema.StatusDone {
		t.Fatalf("already merged: %+v %v", done, err)
	}
	if e.gh.called("SquashMerge") != mergeCalls {
		t.Error("merged a merged PR")
	}
	if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("merge twice: %v", err)
	}
}

func TestMergeWithoutGitHubOrPR(t *testing.T) {
	e := newEnv(t)
	e.svc.gh = nil
	e.toReview("t1")
	if rec, _ := e.svc.GetTask("t1"); rec.Task.PRUrl != "" || !containsLog(rec, "no GitHub client") {
		t.Errorf("record = %+v", rec)
	}
	e.approve("t1")
	if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, ErrNoGitHub) {
		t.Errorf("no github: %v", err)
	}
	e.svc.gh = e.gh
	if _, err := e.svc.Merge(bg, "t1"); !errors.Is(err, ErrNoPR) {
		t.Errorf("no pr: %v", err)
	}
}

func containsLog(rec TaskRecord, sub string) bool {
	for _, l := range rec.Log {
		if strings.Contains(l.Note, sub) {
			return true
		}
	}
	return false
}

func TestDraftPRFailureIsLoggedNotFatal(t *testing.T) {
	e := newEnv(t)
	e.gh.createErr = errors.New("api down")
	rec := e.toReview("t1")
	if rec.Task.PRUrl != "" || !containsLog(rec, "draft PR not created: api down") {
		t.Errorf("record = %+v", rec)
	}
	// A later report can retry publishing: back to working, then review again.
	e.gh.createErr = nil
	if _, err := e.svc.Report(bg, "t1", "working", "retry"); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Report(bg, "t1", "review", "again")
	if err != nil || got.PRUrl == "" {
		t.Fatalf("retry: %+v %v", got, err)
	}
}

func TestReportStateMachine(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.runner.workerFn = func(ctx context.Context, _ pi.WorkerOptions) (*pi.RunResult, error) {
		started <- struct{}{}
		select {
		case <-release:
			return &pi.RunResult{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Report(bg, "t1", "review", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("queued -> review: %v", err)
	}
	if _, err := e.svc.Report(bg, "t1", "done", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("queued -> done: %v", err)
	}
	if _, err := e.svc.Report(bg, "t1", "working", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("queued -> working: %v", err)
	}
	if _, err := e.svc.Report(bg, "t1", "bogus", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("bogus: %v", err)
	}
	if _, err := e.svc.Report(bg, "t1", "progress", " "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("empty progress: %v", err)
	}
	if _, err := e.svc.Report(bg, "ghost", "progress", "x"); !errors.Is(err, ErrNoSuchTask) {
		t.Errorf("ghost: %v", err)
	}

	if _, err := e.svc.Spawn(bg, "t1", ""); err != nil {
		t.Fatal(err)
	}
	<-started
	if rec, err := e.svc.Report(bg, "t1", "progress", "halfway"); err != nil || rec.Status != schema.StatusWorking {
		t.Fatalf("progress: %+v %v", rec, err)
	}
	if _, err := e.svc.Report(bg, "t1", "blocked", "need API key"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.svc.GetTask("t1"); !rec.Blocked || !containsLog(rec, "blocked: need API key") {
		t.Errorf("blocked not recorded: %+v", rec)
	}
	if _, err := e.svc.Report(bg, "t1", "progress", "unblocked"); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.svc.GetTask("t1"); rec.Blocked {
		t.Error("progress should clear blocked")
	}

	// Worker reports review itself while still running: PR opened, run continues until exit.
	rec, err := e.svc.Report(bg, "t1", "review", "ready")
	if err != nil || rec.Status != schema.StatusReview || rec.PRUrl == "" {
		t.Fatalf("review: %+v %v", rec, err)
	}
	close(release)
	e.waitIdle("t1")
	if got, _ := e.svc.GetTask("t1"); got.Task.Status != schema.StatusReview || e.gh.called("CreateDraftPR") != 1 {
		t.Errorf("exit after review changed state: %+v (PRs %d)", got.Task, e.gh.called("CreateDraftPR"))
	}

	if _, err := e.svc.Report(bg, "t1", "done", ""); !errors.Is(err, ErrUseMerge) {
		t.Errorf("done with PR: %v", err)
	}
	if rec, err := e.svc.Report(bg, "t1", "working", "fixing"); err != nil || rec.Status != schema.StatusWorking {
		t.Errorf("review -> working: %+v %v", rec, err)
	}
	if _, err := e.svc.Report(bg, "t1", "closed", "abandon"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Report(bg, "t1", "progress", "late"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("progress on closed: %v", err)
	}
}

func TestReportDoneWithoutPR(t *testing.T) {
	e := newEnv(t)
	e.svc.gh = nil
	e.toReview("t1")
	rec, err := e.svc.Report(bg, "t1", "done", "answered")
	if err != nil || rec.Status != schema.StatusDone || rec.WorktreePath != "" {
		t.Fatalf("done: %+v %v", rec, err)
	}
	if evs, _ := e.svc.Events(0); eventTypes(evs, "t1")[len(eventTypes(evs, "t1"))-1] != schema.EventTaskCompleted {
		t.Error("no completion event")
	}
}

func TestReportQueuedReleasesWorker(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{})
	e.runner.workerFn = func(ctx context.Context, _ pi.WorkerOptions) (*pi.RunResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t1", ""); err != nil {
		t.Fatal(err)
	}
	<-started
	rec, err := e.svc.Report(bg, "t1", "queued", "releasing")
	if err != nil || rec.Status != schema.StatusQueued {
		t.Fatalf("%+v %v", rec, err)
	}
	e.waitIdle("t1")
	if got, _ := e.svc.GetTask("t1"); got.Task.Status != schema.StatusQueued {
		t.Errorf("status = %s", got.Task.Status)
	}
}

func TestCloseTaskStopsWorkerAndKeepsBranch(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{})
	e.runner.workerFn = func(ctx context.Context, _ pi.WorkerOptions) (*pi.RunResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	spawned, err := e.svc.Spawn(bg, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	closed, err := e.svc.CloseTask(bg, "t1", "not needed")
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != schema.StatusClosed || closed.WorktreePath != "" || dirExists(spawned.WorktreePath) {
		t.Errorf("closed = %+v", closed)
	}
	if out := gitIn(t, e.repo, "branch", "--list", "fleet/t1"); !strings.Contains(out, "fleet/t1") {
		t.Error("closing must keep the local branch")
	}
	e.waitIdle("t1")
	if got, _ := e.svc.GetTask("t1"); got.Task.Status != schema.StatusClosed {
		t.Errorf("worker exit overwrote closed state: %s", got.Task.Status)
	}
	if _, err := e.svc.CloseTask(bg, "t1", ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("close twice: %v", err)
	}
}

func TestBudgetEnforcement(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{MaxUSD: ptr(0.3)}); err != nil {
		t.Fatal(err)
	}
	e.toReview("t1") // costs 0.25 -> below the limit
	if _, err := e.svc.Delegate("alpha", "t2", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t2", ""); err != nil {
		t.Fatalf("0.25 < 0.30 must still allow spawn: %v", err)
	}
	e.waitStatus("t2", schema.StatusReview)

	// 0.5 spent now: space budget exceeded
	if _, err := e.svc.Delegate("alpha", "t3", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Spawn(bg, "t3", "")
	if !errors.Is(err, pi.ErrBudgetExceeded) {
		t.Fatalf("spawn over budget: %v", err)
	}
	if rec, _ := e.svc.GetTask("t3"); rec.Task.Status != schema.StatusQueued || rec.Task.WorktreePath != "" {
		t.Errorf("rejected spawn left state behind: %+v", rec.Task)
	}
	if _, err := e.svc.Review(bg, "t1"); !errors.Is(err, pi.ErrBudgetExceeded) {
		t.Errorf("review over budget: %v", err)
	}

	rep, err := e.svc.Cost("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Usage.Cost.Total < 0.4999 || rep.Usage.Cost.Total > 0.5001 || len(rep.Statuses) != 1 || rep.Statuses[0].State != pi.BudgetExceeded {
		t.Errorf("cost report = %+v", rep)
	}
	if rep, err = e.svc.Cost("alpha", "t1"); err != nil || len(rep.Statuses) != 2 || rep.Usage.Cost.Total < 0.2499 {
		t.Errorf("task cost = %+v %v", rep, err)
	}
	if _, err := e.svc.Cost("alpha", "ghost"); !errors.Is(err, ErrNoSuchTask) {
		t.Errorf("cost ghost: %v", err)
	}
	if _, err := e.svc.Cost("ghost", ""); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("cost ghost space: %v", err)
	}

	// Raising the limit unblocks it.
	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{MaxUSD: ptr(10.0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t3", ""); err != nil {
		t.Fatalf("after raising budget: %v", err)
	}
}

func TestTaskAndFleetBudgets(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{TaskMaxUSD: ptr(0.2)}); err != nil {
		t.Fatal(err)
	}
	e.toReview("t1") // 0.25 > 0.2 per-task limit
	if _, err := e.svc.Review(bg, "t1"); !errors.Is(err, pi.ErrBudgetExceeded) {
		t.Errorf("task budget: %v", err)
	}
	// Other tasks are unaffected by t1's overspend.
	if _, err := e.svc.Delegate("alpha", "t2", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t2", ""); err != nil {
		t.Errorf("fresh task blocked: %v", err)
	}
	e.waitStatus("t2", schema.StatusReview)

	if err := e.svc.SetSpaceBudget("alpha", BudgetUpdate{TaskMaxUSD: ptr(0.0)}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetFleetBudget(0.4); err != nil { // 0.5 spent already
		t.Fatal(err)
	}
	if _, err := e.svc.Delegate("alpha", "t3", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t3", ""); !errors.Is(err, pi.ErrBudgetExceeded) || !strings.Contains(err.Error(), "fleet") {
		t.Errorf("fleet budget: %v", err)
	}
}

func TestRestartRecoversState(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{})
	e.runner.workerFn = func(ctx context.Context, _ pi.WorkerOptions) (*pi.RunResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	spawned, err := e.svc.Spawn(bg, "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
	if _, err := e.svc.Delegate("alpha", "late", "x", nil); !errors.Is(err, ErrShutdown) {
		t.Errorf("delegate after shutdown: %v", err)
	}

	// The interrupted worker did not change the state.
	svc2 := e.open()
	defer svc2.Shutdown(bg)
	rec, err := svc2.GetTask("t1")
	if err != nil || rec.Task.Status != schema.StatusWorking || rec.Task.WorktreePath != spawned.WorktreePath {
		t.Fatalf("recovered = %+v %v", rec.Task, err)
	}
	if !containsLog(rec, "interrupted") {
		t.Errorf("log = %+v", rec.Log)
	}

	// and it can be relaunched in the same worktree.
	e.runner.mu.Lock()
	e.runner.workerFn = nil
	e.runner.mu.Unlock()
	if _, err := svc2.Spawn(bg, "t1", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "relaunched worker to report review", func() bool {
		r, _ := svc2.GetTask("t1")
		return r.Task.Status == schema.StatusReview
	})
}

func TestCorruptStateIsRejected(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Delegate("alpha", "t1", "x", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.svc.tasksPath(), []byte(`{"version":1,"tasks":[{"task":{"id":"x"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Paths: e.paths, Worktrees: e.wt, Runner: e.runner}); err == nil {
		t.Error("invalid stored task accepted")
	}
	if err := os.WriteFile(e.svc.tasksPath(), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Paths: e.paths, Worktrees: e.wt, Runner: e.runner}); err == nil {
		t.Error("garbage state accepted")
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	e := newEnv(t)
	ch, cancel := e.svc.Subscribe(8)
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Type != schema.EventTaskProgress || ev.TaskID != "t1" || ev.SpaceID != "alpha" || ev.Role != schema.RoleLead || ev.Timestamp.IsZero() {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-ch; ok {
		t.Error("channel not closed after cancel")
	}
	// A subscriber that never reads must not block the engine.
	_, cancel2 := e.svc.Subscribe(1)
	defer cancel2()
	for i := 0; i < 5; i++ {
		if _, err := e.svc.Delegate("alpha", "slow"+string(rune('a'+i)), "x", nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJiraFailureIsLogged(t *testing.T) {
	e := newEnv(t)
	e.jira.err = errors.New("jira down")
	if _, err := e.svc.Delegate("alpha", "t1", "x", []string{"c"}, WithJiraKey("PROJ-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Spawn(bg, "t1", ""); err != nil {
		t.Fatalf("jira failure must not fail spawn: %v", err)
	}
	rec := e.waitStatus("t1", schema.StatusReview)
	waitFor(t, "jira note", func() bool {
		r, _ := e.svc.GetTask("t1")
		return containsLog(r, "jira PROJ-1") && len(r.Log) > 0
	})
	_ = rec
}

func TestEventsEmptyLog(t *testing.T) {
	e := newEnv(t)
	evs, err := e.svc.Events(10)
	if err != nil || evs == nil || len(evs) != 0 {
		t.Errorf("events = %v, %v", evs, err)
	}
}

func TestLogIsBounded(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Delegate("alpha", "t1", "x", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxLogEntries+20; i++ {
		e.svc.noteLog("t1", schema.RoleLead, "n")
	}
	rec, _ := e.svc.GetTask("t1")
	if len(rec.Log) != maxLogEntries {
		t.Errorf("log length = %d", len(rec.Log))
	}
}
