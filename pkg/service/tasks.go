package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/git"
	"fleet/pkg/integrations"
	"fleet/pkg/driver"
	"fleet/pkg/schema"
)

// Report statuses accepted in addition to the schema statuses.
const (
	ReportProgress = "progress" // log a note, no state change
	ReportBlocked  = "blocked"  // flag the task as blocked, no state change
)

// workerRun is a running Pi worker.
type workerRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed once the worker and its bookkeeping finished
}

// errSkip aborts a mutate callback without an error.
var errSkip = errors.New("skip")

// ---------------------------------------------------------------- helpers

func (s *Service) getLocked(id string) (*TaskRecord, error) {
	r, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoSuchTask, id)
	}
	return r, nil
}

func (s *Service) taskInSpace(space, id string) (TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getLocked(id)
	if err != nil {
		return TaskRecord{}, err
	}
	if r.Task.SpaceID != space {
		return TaskRecord{}, fmt.Errorf("%w: %q in space %q", ErrNoSuchTask, id, space)
	}
	return r.clone(), nil
}

// claim marks the task busy with op, failing with ErrBusy when it already is.
// The returned function releases the claim.
func (s *Service) claim(id, op string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrShutdown
	}
	if cur, ok := s.busy[id]; ok {
		return nil, fmt.Errorf("%w: %s in progress", ErrBusy, cur)
	}
	s.busy[id] = op
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.busy, id)
			s.mu.Unlock()
		})
	}, nil
}

// mutate applies fn to a copy of the task, validates and persists it. When fn
// returns errSkip nothing is written and the current record is returned with
// a nil error.
func (s *Service) mutate(id string, fn func(*TaskRecord) error) (TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getLocked(id)
	if err != nil {
		return TaskRecord{}, err
	}
	c := r.clone()
	if err := fn(&c); err != nil {
		if errors.Is(err, errSkip) {
			return c, nil
		}
		return TaskRecord{}, err
	}
	if err := c.Task.Validate(); err != nil {
		return TaskRecord{}, fmt.Errorf("service: task %s: %w", id, err)
	}
	c.UpdatedAt = s.now().UTC()
	s.tasks[id] = &c
	if err := s.saveTasksLocked(); err != nil {
		s.tasks[id] = r
		return TaskRecord{}, err
	}
	return c.clone(), nil
}

func (s *Service) addLog(r *TaskRecord, role schema.AgentRole, note string) {
	r.Log = append(r.Log, LogEntry{Time: s.now().UTC(), Role: role, Status: r.Task.Status, Note: note})
	if over := len(r.Log) - maxLogEntries; over > 0 {
		r.Log = append(r.Log[:0], r.Log[over:]...)
	}
}

// transition moves r to next if the state machine allows it and logs note.
func (s *Service) transition(r *TaskRecord, next schema.Status, role schema.AgentRole, note string) error {
	if !r.Task.Status.CanTransition(next) {
		return fmt.Errorf("%w: task %s is %s, cannot become %s", ErrInvalidTransition, r.Task.ID, r.Task.Status, next)
	}
	r.Task.Status = next
	if note == "" {
		note = "-> " + string(next)
	}
	s.addLog(r, role, note)
	return nil
}

func (s *Service) noteLog(id string, role schema.AgentRole, note string) {
	if _, err := s.mutate(id, func(r *TaskRecord) error { s.addLog(r, role, note); return nil }); err != nil {
		_ = err // the task may have been removed meanwhile; the note is best effort
	}
}

func (s *Service) taskEvent(t schema.EventType, r *TaskRecord, role schema.AgentRole, msg string) schema.AgentEvent {
	return schema.AgentEvent{Type: t, TaskID: r.Task.ID, SpaceID: r.Task.SpaceID, Role: role, Message: msg}
}

func (s *Service) progress(r *TaskRecord, role schema.AgentRole, msg string, spend float64) {
	e := s.taskEvent(schema.EventTaskProgress, r, role, msg)
	if spend > 0 {
		e.SpendUSD = &spend
	}
	s.emit(e)
}

// joinCtx returns a context cancelled when either ctx or the service ends.
func (s *Service) joinCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return c, func() { stop(); cancel() }
}

func (s *Service) jiraTransition(ctx context.Context, r *TaskRecord, target string) {
	if s.jira == nil || r.Task.JiraKey == "" {
		return
	}
	if err := s.jira.TransitionIssue(ctx, r.Task.JiraKey, target); err != nil {
		s.noteLog(r.Task.ID, schema.RoleLead, fmt.Sprintf("jira %s -> %q failed: %v", r.Task.JiraKey, target, err))
	}
}

func (s *Service) agentConfig(role agent.Role) agent.Config {
	cfg := agent.DefaultConfig(role)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch role {
	case agent.RoleLead:
		if s.cfg.Models.Lead != "" {
			cfg.Model = s.cfg.Models.Lead
		}
	case agent.RoleWorker:
		if s.cfg.Models.Worker != "" {
			cfg.Model = s.cfg.Models.Worker
		}
	case agent.RoleReviewer:
		if s.cfg.Models.Reviewer != "" {
			cfg.Model = s.cfg.Models.Reviewer
		}
	}
	return cfg
}

// ---------------------------------------------------------------- queries

// GetTask returns a task with its log and reviews.
func (s *Service) GetTask(id string) (TaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.getLocked(id)
	if err != nil {
		return TaskRecord{}, err
	}
	return r.clone(), nil
}

// Diff returns the unified diff of the task's worktree against its base. It is
// empty while the task has no worktree.
func (s *Service) Diff(ctx context.Context, id string) (string, error) {
	rec, err := s.GetTask(id)
	if err != nil {
		return "", err
	}
	if rec.Task.WorktreePath == "" {
		return "", nil
	}
	ctx, cancel := s.joinCtx(ctx)
	defer cancel()
	return s.wt.Diff(ctx, rec.Task.WorktreePath, rec.BaseRef)
}

// TaskFilter narrows ListTasks. Empty fields match everything.
type TaskFilter struct {
	Space  string        `json:"space,omitempty"`
	Status schema.Status `json:"status,omitempty"`
}

// ListTasks returns matching tasks in creation order.
func (s *Service) ListTasks(f TaskFilter) []schema.Task {
	s.mu.Lock()
	recs := make([]*TaskRecord, 0, len(s.tasks))
	for _, r := range s.tasks {
		if (f.Space == "" || r.Task.SpaceID == f.Space) && (f.Status == "" || r.Task.Status == f.Status) {
			recs = append(recs, r)
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].CreatedAt.Equal(recs[j].CreatedAt) {
			return recs[i].CreatedAt.Before(recs[j].CreatedAt)
		}
		return recs[i].Task.ID < recs[j].Task.ID
	})
	out := make([]schema.Task, len(recs))
	for i, r := range recs {
		out[i] = r.clone().Task
	}
	s.mu.Unlock()
	return out
}

// ---------------------------------------------------------------- Delegate

type delegateOpts struct{ title, project, jiraKey string }

// DelegateOption customises Delegate.
type DelegateOption func(*delegateOpts)

// WithTitle sets the task title (default: the first line of the brief).
func WithTitle(t string) DelegateOption { return func(o *delegateOpts) { o.title = t } }

// WithProject selects the project inside the space (default: the only one).
func WithProject(id string) DelegateOption { return func(o *delegateOpts) { o.project = id } }

// WithJiraKey links the task to a Jira issue, which is moved to In Progress
// on spawn and Done on completion.
func WithJiraKey(k string) DelegateOption { return func(o *delegateOpts) { o.jiraKey = k } }

func defaultTitle(brief string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(brief), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 80 {
		line = string(r[:77]) + "..."
	}
	return line
}

// Delegate records a new task in the space as queued. task is the task ID: a
// short slug unique across the fleet.
func (s *Service) Delegate(space, task, brief string, criteria []string, opts ...DelegateOption) (schema.Task, error) {
	var o delegateOpts
	for _, f := range opts {
		f(&o)
	}
	if err := errors.Join(config.ValidateID(task), agent.ValidateName("task", task)); err != nil {
		return schema.Task{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return schema.Task{}, fmt.Errorf("%w: empty brief", ErrInvalidArgument)
	}
	crit := make([]string, 0, len(criteria))
	for _, c := range criteria {
		if c = strings.TrimSpace(c); c != "" {
			crit = append(crit, c)
		}
	}
	if o.jiraKey != "" && !integrations.ValidIssueKey(o.jiraKey) {
		return schema.Task{}, fmt.Errorf("%w: %q", integrations.ErrInvalidIssueKey, o.jiraKey)
	}
	title := strings.TrimSpace(o.title)
	if title == "" {
		title = defaultTitle(brief)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return schema.Task{}, ErrShutdown
	}
	sp, ok := s.cfg.Space(space)
	if !ok {
		s.mu.Unlock()
		return schema.Task{}, fmt.Errorf("%w: %q", ErrNoSuchSpace, space)
	}
	project := o.project
	switch {
	case project != "":
		if _, ok := sp.Project(project); !ok {
			s.mu.Unlock()
			return schema.Task{}, fmt.Errorf("%w: space %q has no project %q", ErrNoProject, space, project)
		}
	case len(sp.Projects) == 1:
		project = sp.Projects[0].ID
	case len(sp.Projects) > 1:
		s.mu.Unlock()
		return schema.Task{}, fmt.Errorf("%w: space %q has several projects; choose one", ErrNoProject, space)
	}
	if _, dup := s.tasks[task]; dup {
		s.mu.Unlock()
		return schema.Task{}, fmt.Errorf("%w: %q", ErrTaskExists, task)
	}
	now := s.now().UTC()
	rec := &TaskRecord{
		Task: schema.Task{
			ID: task, SpaceID: space, ProjectID: project, Title: title, Brief: brief,
			AcceptanceCriteria: crit, Status: schema.StatusQueued, JiraKey: o.jiraKey,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := rec.Task.Validate(); err != nil {
		s.mu.Unlock()
		return schema.Task{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s.addLog(rec, schema.RoleLead, "delegated")
	s.tasks[task] = rec
	if err := s.saveTasksLocked(); err != nil {
		delete(s.tasks, task)
		s.mu.Unlock()
		return schema.Task{}, err
	}
	s.refreshBudgetsLocked()
	out := rec.clone()
	s.mu.Unlock()

	s.progress(&out, schema.RoleLead, "task delegated: "+title, 0)
	return out.Task, nil
}

// ---------------------------------------------------------------- Spawn

// Spawn creates the task's worktree (on branch, default fleet/<task>) under
// the XDG data directory and launches a Pi worker in it. The task becomes
// working and the call returns once the worker is started. Spawning a working
// task that has no running worker (after a restart, or after requested
// changes) relaunches it in the existing worktree.
func (s *Service) Spawn(ctx context.Context, task, branch string) (schema.Task, error) {
	release, err := s.claim(task, "spawn")
	if err != nil {
		return schema.Task{}, err
	}
	defer release()

	s.mu.Lock()
	r, err := s.getLocked(task)
	if err != nil {
		s.mu.Unlock()
		return schema.Task{}, err
	}
	rec := r.clone()
	_, running := s.runs[task]
	var project config.Project
	if sp, ok := s.cfg.Space(rec.Task.SpaceID); ok {
		if p, ok := sp.Project(rec.Task.ProjectID); ok {
			project = *p
		}
	}
	s.mu.Unlock()

	switch {
	case running:
		return schema.Task{}, fmt.Errorf("%w: worker already running for %s", ErrBusy, task)
	case rec.Task.Status != schema.StatusQueued && rec.Task.Status != schema.StatusWorking:
		return schema.Task{}, fmt.Errorf("%w: task %s is %s, cannot spawn", ErrInvalidTransition, task, rec.Task.Status)
	}
	if err := s.checkBudget(rec.Task.SpaceID, task); err != nil {
		return schema.Task{}, err
	}

	created := false
	wtPath, br, repo, baseRef, baseBranch := rec.Task.WorktreePath, rec.Task.Branch, rec.Repo, rec.BaseRef, rec.BaseBranch
	if wtPath != "" {
		if _, err := os.Stat(wtPath); err != nil {
			return schema.Task{}, fmt.Errorf("service: worktree %s is gone: %w", wtPath, err)
		}
		if branch != "" && branch != br {
			return schema.Task{}, fmt.Errorf("%w: task already uses branch %q", ErrInvalidArgument, br)
		}
	} else {
		if project.ID == "" {
			return schema.Task{}, fmt.Errorf("%w: task %s", ErrNoProject, task)
		}
		w, err := s.wt.Create(ctx, git.CreateOptions{
			Repo: project.Path, SpaceID: rec.Task.SpaceID, TaskID: task, Branch: branch, Base: project.BaseBranch,
		})
		if err != nil {
			return schema.Task{}, fmt.Errorf("service: create worktree: %w", err)
		}
		created = true
		wtPath, br, repo, baseRef = w.Path, w.Branch, w.Repo, w.Base
		baseBranch = project.BaseBranch
		if baseBranch == "" {
			baseBranch = strings.TrimPrefix(w.Base, "origin/")
		}
	}

	prevStatus := rec.Task.Status
	updated, err := s.mutate(task, func(r *TaskRecord) error {
		if r.Task.Status != prevStatus {
			return fmt.Errorf("%w: task %s changed to %s while spawning", ErrInvalidTransition, task, r.Task.Status)
		}
		r.Task.Branch, r.Task.WorktreePath, r.Repo, r.BaseRef, r.BaseBranch = br, wtPath, repo, baseRef, baseBranch
		r.Task.AssignedWorker = "worker-" + task
		r.Blocked = false
		if r.Task.Status == schema.StatusQueued {
			return s.transition(r, schema.StatusWorking, schema.RoleLead, "worker spawned on "+br)
		}
		s.addLog(r, schema.RoleLead, "worker relaunched on "+br)
		return nil
	})
	if err != nil {
		if created {
			_ = s.wt.Remove(context.WithoutCancel(ctx), &git.Worktree{Repo: repo, Path: wtPath, Branch: br, SpaceID: rec.Task.SpaceID, TaskID: task}, true)
		}
		return schema.Task{}, err
	}

	runCtx, cancel := context.WithCancel(s.ctx)
	run := &workerRun{ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.runs[task] = run
	s.mu.Unlock()
	s.wg.Add(1)
	go s.runWorker(run, updated)

	s.emit(s.taskEvent(schema.EventTaskStarted, &updated, schema.RoleWorker, "worker started on "+br))
	s.jiraTransition(ctx, &updated, "In Progress")
	return updated.Task, nil
}

func workerBrief(r *TaskRecord) string {
	if r.Feedback == "" {
		return r.Task.Brief
	}
	return r.Task.Brief + "\n\n## Reviewer feedback to address\n\n" + r.Feedback
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + strings.ToValidUTF8(s[len(s)-n:], "")
}

func (s *Service) runWorker(run *workerRun, rec TaskRecord) {
	defer s.wg.Done()
	defer close(run.done)
	defer run.cancel()
	id := rec.Task.ID
	defer func() { // still "running" while the exit is being processed
		s.mu.Lock()
		if s.runs[id] == run {
			delete(s.runs, id)
		}
		s.mu.Unlock()
	}()

	res, runErr := s.driver.RunWorker(run.ctx, driver.WorkerRequest{
		Space:      rec.Task.SpaceID,
		Task:       id,
		Title:      rec.Task.Title,
		Brief:      workerBrief(&rec),
		Branch:     rec.Task.Branch,
		BaseBranch: rec.BaseBranch,
		Acceptance: rec.Task.AcceptanceCriteria,
		Worktree:   rec.Task.WorktreePath,
	})
	delta, serr := s.syncSpend(rec.Task.SpaceID, id)
	if serr != nil {
		s.noteLog(id, schema.RoleWorker, "recording spend failed: "+serr.Error())
	}
	switch interrupted := run.ctx.Err() != nil; {
	case interrupted:
		s.noteLog(id, schema.RoleWorker, "worker interrupted")
		s.progress(&rec, schema.RoleWorker, "worker interrupted", delta)
	case runErr != nil:
		msg := "worker failed: " + runErr.Error()
		s.progress(&rec, schema.RoleWorker, msg, delta)
		_, err := s.mutate(id, func(r *TaskRecord) error {
			if r.Task.Status != schema.StatusWorking {
				return errSkip
			}
			return s.transition(r, schema.StatusQueued, schema.RoleWorker, msg)
		})
		if err != nil {
			s.noteLog(id, schema.RoleWorker, "state update failed: "+err.Error())
		}
	default:
		summary := ""
		if res != nil {
			summary = tail(res.Output, 1000)
		}
		s.progress(&rec, schema.RoleWorker, "worker finished", delta)
		if summary == "" {
			summary = "worker finished"
		}
		// The worker may already have reported review itself; that is fine.
		if _, err := s.report(s.ctx, id, string(schema.StatusReview), summary, schema.RoleWorker); err != nil &&
			!errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrBusy) {
			s.noteLog(id, schema.RoleWorker, "automatic review report failed: "+err.Error())
		}
	}
}

// stopRun cancels the task's worker and waits for it to exit.
func (s *Service) stopRun(ctx context.Context, id string) error {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("service: waiting for worker of %s: %w", id, ctx.Err())
	}
}

// ---------------------------------------------------------------- Report

// Report records a status update for a task. status is one of the schema
// statuses (working, review, queued, done, closed) or "progress"/"blocked".
//
//	progress  log note, no state change (note required)
//	blocked   flag the task as blocked (note required)
//	working   review -> working: changes are being made
//	review    working -> review: pushes the branch and opens a draft PR when GitHub is configured
//	queued    working -> queued: release the task
//	done      review -> done for tasks without a PR (otherwise use Merge)
//	closed    abandon the task
func (s *Service) Report(ctx context.Context, task, status, note string) (schema.Task, error) {
	r, err := s.report(ctx, task, status, note, schema.RoleWorker)
	return r.Task, err
}

func (s *Service) report(ctx context.Context, id, status, note string, role schema.AgentRole) (TaskRecord, error) {
	note = strings.TrimSpace(note)
	switch status {
	case ReportProgress, ReportBlocked:
		if note == "" {
			return TaskRecord{}, fmt.Errorf("%w: %s needs a note", ErrInvalidArgument, status)
		}
		rec, err := s.mutate(id, func(r *TaskRecord) error {
			if r.Task.Status.Terminal() {
				return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, id, r.Task.Status)
			}
			r.Blocked = status == ReportBlocked
			s.addLog(r, role, status+": "+note)
			return nil
		})
		if err != nil {
			return TaskRecord{}, err
		}
		msg := note
		if status == ReportBlocked {
			msg = "blocked: " + note
		}
		s.progress(&rec, role, msg, 0)
		return rec, nil

	case string(schema.StatusWorking):
		rec, err := s.mutate(id, func(r *TaskRecord) error {
			if r.Task.Status != schema.StatusReview {
				return fmt.Errorf("%w: task %s is %s; use spawn to start work", ErrInvalidTransition, id, r.Task.Status)
			}
			r.Blocked = false
			return s.transition(r, schema.StatusWorking, role, note)
		})
		if err != nil {
			return TaskRecord{}, err
		}
		s.progress(&rec, role, "working again"+suffix(note), 0)
		return rec, nil

	case string(schema.StatusQueued):
		rec, err := s.mutate(id, func(r *TaskRecord) error { return s.transition(r, schema.StatusQueued, role, note) })
		if err != nil {
			return TaskRecord{}, err
		}
		if err := s.stopRun(ctx, id); err != nil {
			return rec, err
		}
		s.progress(&rec, role, "task released"+suffix(note), 0)
		return rec, nil

	case string(schema.StatusReview):
		return s.reportReview(ctx, id, note, role)

	case string(schema.StatusDone):
		return s.reportDone(ctx, id, note, role)

	case string(schema.StatusClosed):
		_, err := s.CloseTask(ctx, id, note)
		rec, _ := s.GetTask(id)
		return rec, err
	}
	return TaskRecord{}, fmt.Errorf("%w: unknown status %q", ErrInvalidArgument, status)
}

func suffix(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

func (s *Service) reportReview(ctx context.Context, id, note string, role schema.AgentRole) (TaskRecord, error) {
	release, err := s.claim(id, "report review")
	if err != nil {
		return TaskRecord{}, err
	}
	defer release()
	rec, err := s.GetTask(id)
	if err != nil {
		return TaskRecord{}, err
	}
	if rec.Task.Status != schema.StatusWorking {
		return TaskRecord{}, fmt.Errorf("%w: task %s is %s, cannot request review", ErrInvalidTransition, id, rec.Task.Status)
	}

	ctx, cancel := s.joinCtx(ctx)
	defer cancel()
	prURL, ghRepo, pubNote := s.publish(ctx, &rec)

	rec, err = s.mutate(id, func(r *TaskRecord) error {
		if r.Task.Status != schema.StatusWorking {
			return fmt.Errorf("%w: task %s is %s, cannot request review", ErrInvalidTransition, id, r.Task.Status)
		}
		if prURL != "" {
			r.Task.PRUrl, r.GitHubRepo = prURL, ghRepo
		}
		r.Blocked = false
		if err := s.transition(r, schema.StatusReview, role, note); err != nil {
			return err
		}
		if pubNote != "" {
			s.addLog(r, schema.RoleLead, pubNote)
		}
		return nil
	})
	release() // the new state is visible: let the caller's next call in
	if err != nil {
		return TaskRecord{}, err
	}
	e := s.taskEvent(schema.EventTaskReviewRequested, &rec, schema.RoleWorker, note)
	e.PRUrl = rec.Task.PRUrl
	s.emit(e)
	return rec, nil
}

// publish pushes the task branch and opens (or finds) the draft PR. It never
// fails the caller: problems come back as a log note.
func (s *Service) publish(ctx context.Context, rec *TaskRecord) (prURL, ghRepo, note string) {
	if s.gh == nil {
		return "", "", "no GitHub client configured: branch not pushed, no PR opened"
	}
	if rec.Task.WorktreePath == "" || rec.Task.Branch == "" {
		return "", "", "task has no worktree: nothing to publish"
	}
	repo := rec.GitHubRepo
	if repo == "" {
		var err error
		if repo, err = s.wt.RemoteRepo(ctx, rec.Repo); err != nil {
			return "", "", "draft PR not created: " + err.Error()
		}
	}
	if err := s.wt.Push(ctx, rec.Task.WorktreePath, rec.Task.Branch); err != nil {
		return "", "", "push failed: " + err.Error()
	}
	if rec.Task.PRUrl != "" {
		return rec.Task.PRUrl, repo, "pushed new commits to " + rec.Task.Branch
	}
	pr, err := s.gh.CreateDraftPR(ctx, repo, integrations.NewPR{
		Title: rec.Task.Title, Body: prBody(rec), Head: rec.Task.Branch, Base: rec.BaseBranch,
	})
	if err != nil {
		return "", "", "draft PR not created: " + err.Error()
	}
	return pr.URL, repo, "draft PR opened: " + pr.URL
}

func prBody(r *TaskRecord) string {
	var b strings.Builder
	b.WriteString(r.Task.Brief)
	if len(r.Task.AcceptanceCriteria) > 0 {
		b.WriteString("\n\n## Acceptance criteria\n\n")
		for _, c := range r.Task.AcceptanceCriteria {
			b.WriteString("- [ ] " + c + "\n")
		}
	}
	b.WriteString("\n_Fleet task `" + r.Task.ID + "`._\n")
	return b.String()
}

func (s *Service) reportDone(ctx context.Context, id, note string, role schema.AgentRole) (TaskRecord, error) {
	release, err := s.claim(id, "done")
	if err != nil {
		return TaskRecord{}, err
	}
	defer release()
	rec, err := s.GetTask(id)
	if err != nil {
		return TaskRecord{}, err
	}
	if rec.Task.Status != schema.StatusReview {
		return TaskRecord{}, fmt.Errorf("%w: task %s is %s; only tasks in review can be done", ErrInvalidTransition, id, rec.Task.Status)
	}
	if rec.Task.PRUrl != "" {
		return TaskRecord{}, fmt.Errorf("%w: %s", ErrUseMerge, rec.Task.PRUrl)
	}
	return s.finalize(ctx, id, role, note)
}

// finalize marks a review-state task done, cleans up and emits completion.
// The task is done even when cleanup fails; that is reported as ErrCleanup
// alongside the updated record.
func (s *Service) finalize(ctx context.Context, id string, role schema.AgentRole, note string) (TaskRecord, error) {
	rec, err := s.mutate(id, func(r *TaskRecord) error {
		r.Blocked, r.Feedback = false, ""
		return s.transition(r, schema.StatusDone, role, note)
	})
	if err != nil {
		return TaskRecord{}, err
	}
	e := s.taskEvent(schema.EventTaskCompleted, &rec, role, note)
	e.PRUrl = rec.Task.PRUrl
	s.emit(e)
	s.jiraTransition(ctx, &rec, "Done")

	cerr := s.cleanup(ctx, &rec, true)
	for _, rl := range []agent.Role{agent.RoleWorker, agent.RoleReviewer} {
		if dir, derr := agent.SessionDir(rl, rec.Task.SpaceID, id); derr == nil {
			// Spend is already in the ledger; sessions are removed per role policy.
			if parser, ok := s.driver.(driver.SessionParser); ok {
				if sess, err := parser.ParseSessions(dir); err == nil {
					_, _ = s.tracker.IngestSessions(rec.Task.SpaceID, id, rl, sess)
				}
			}
			pol := agent.DefaultConfig(rl).Cleanup
			cerr = errors.Join(cerr, agent.Cleaner{}.Apply(pol, agent.OutcomeSucceeded, agent.Targets{SessionDir: dir}))
		}
	}
	if _, serr := s.syncSpend(rec.Task.SpaceID, id); serr != nil {
		cerr = errors.Join(cerr, serr)
	}
	out, _ := s.GetTask(id)
	if cerr != nil {
		s.noteLog(id, schema.RoleLead, "cleanup: "+cerr.Error())
		return out, fmt.Errorf("%w: %w", ErrCleanup, cerr)
	}
	return out, nil
}

// cleanup removes the task's worktree (and branch when deleteBranch) and
// clears the stored path.
func (s *Service) cleanup(ctx context.Context, rec *TaskRecord, deleteBranch bool) error {
	if rec.Task.WorktreePath == "" {
		return nil
	}
	w := &git.Worktree{
		Repo: rec.Repo, Path: rec.Task.WorktreePath, Branch: rec.Task.Branch,
		SpaceID: rec.Task.SpaceID, TaskID: rec.Task.ID,
	}
	if err := s.wt.Remove(ctx, w, deleteBranch); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	_, err := s.mutate(rec.Task.ID, func(r *TaskRecord) error {
		r.Task.WorktreePath = ""
		return nil
	})
	return err
}

// ---------------------------------------------------------------- Close

// CloseTask abandons a task: its worker is stopped and its worktree removed.
// The local branch is kept so no work is lost.
func (s *Service) CloseTask(ctx context.Context, id, note string) (schema.Task, error) {
	release, err := s.claim(id, "close")
	if err != nil {
		return schema.Task{}, err
	}
	defer release()
	note = strings.TrimSpace(note)
	rec, err := s.mutate(id, func(r *TaskRecord) error {
		return s.transition(r, schema.StatusClosed, schema.RoleLead, note)
	})
	if err != nil {
		return schema.Task{}, err
	}
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	stopErr := s.stopRun(waitCtx, id)
	s.progress(&rec, schema.RoleLead, "task closed"+suffix(note), 0)
	if stopErr != nil {
		return rec.Task, stopErr
	}
	if err := s.cleanup(waitCtx, &rec, false); err != nil {
		s.noteLog(id, schema.RoleLead, "cleanup: "+err.Error())
		out, _ := s.GetTask(id)
		return out.Task, fmt.Errorf("%w: %w", ErrCleanup, err)
	}
	out, _ := s.GetTask(id)
	return out.Task, nil
}

// ---------------------------------------------------------------- Review

// ReviewResult is the outcome of Review.
type ReviewResult struct {
	Verdict schema.ReviewVerdict `json:"verdict"`
	Detail  agent.ReviewVerdict  `json:"detail"`
	SHA     string               `json:"sha"`
	CostUSD float64              `json:"cost_usd"`
}

// Review runs the reviewer agent over the unified diff of the task's worktree
// against its base and the acceptance criteria. An approval keeps the task in
// review, ready for Merge; any other verdict sends it back to working with the
// findings stored as feedback for the next worker run.
func (s *Service) Review(ctx context.Context, task string) (ReviewResult, error) {
	release, err := s.claim(task, "review")
	if err != nil {
		return ReviewResult{}, err
	}
	defer release()
	rec, err := s.GetTask(task)
	if err != nil {
		return ReviewResult{}, err
	}
	switch {
	case rec.Task.Status != schema.StatusReview:
		return ReviewResult{}, fmt.Errorf("%w: task %s is %s, cannot review", ErrInvalidTransition, task, rec.Task.Status)
	case len(rec.Task.AcceptanceCriteria) == 0:
		return ReviewResult{}, fmt.Errorf("%w: %s", ErrNoCriteria, task)
	case rec.Task.WorktreePath == "":
		return ReviewResult{}, fmt.Errorf("service: task %s has no worktree to review", task)
	}
	if err := s.checkBudget(rec.Task.SpaceID, task); err != nil {
		return ReviewResult{}, err
	}

	ctx, cancel := s.joinCtx(ctx)
	defer cancel()
	sha, err := s.wt.Head(ctx, rec.Task.WorktreePath)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("service: read head: %w", err)
	}
	diff, err := s.wt.Diff(ctx, rec.Task.WorktreePath, rec.BaseRef)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("service: diff: %w", err)
	}
	if strings.TrimSpace(diff) == "" {
		return ReviewResult{}, fmt.Errorf("%w: %s", ErrEmptyDiff, task)
	}

	before := s.tracker.TaskUsage(rec.Task.SpaceID, task).Cost.Total
	res, runErr := s.driver.RunReviewer(ctx, driver.ReviewerRequest{
		Space:    rec.Task.SpaceID,
		Task:     task,
		Title:    rec.Task.Title,
		Criteria: rec.Task.AcceptanceCriteria,
		Diff:     diff,
		Worktree: rec.Task.WorktreePath,
	})
	var verdict *agent.ReviewVerdict
	if res != nil {
		verdict = res.Verdict
	}
	_, serr := s.syncSpend(rec.Task.SpaceID, task)
	cost := s.tracker.TaskUsage(rec.Task.SpaceID, task).Cost.Total - before
	if runErr != nil {
		return ReviewResult{CostUSD: cost}, errors.Join(fmt.Errorf("service: review %s: %w", task, runErr), serr)
	}

	sv := toSchemaVerdict(verdict)
	updated, err := s.mutate(task, func(r *TaskRecord) error {
		r.Reviews = append(r.Reviews, ReviewRecord{
			Time: s.now().UTC(), Verdict: verdict.Verdict, Summary: verdict.Summary,
			Issues: verdict.Issues, Criteria: verdict.Criteria, SHA: sha, CostUSD: cost,
		})
		note := fmt.Sprintf("review: %s: %s", verdict.Verdict, verdict.Summary)
		if verdict.Approved() {
			r.Feedback = ""
			s.addLog(r, schema.RoleReviewer, note)
			return nil
		}
		r.Feedback = feedbackText(verdict)
		if r.Task.Status != schema.StatusReview {
			return fmt.Errorf("%w: task %s is %s", ErrInvalidTransition, task, r.Task.Status)
		}
		return s.transition(r, schema.StatusWorking, schema.RoleReviewer, note)
	})
	if err != nil {
		return ReviewResult{}, errors.Join(err, serr)
	}
	e := s.taskEvent(schema.EventReviewVerdict, &updated, schema.RoleReviewer, verdict.Summary)
	e.Verdict = &sv
	if cost > 0 {
		e.SpendUSD = &cost
	}
	s.emit(e)
	return ReviewResult{Verdict: sv, Detail: *verdict, SHA: sha, CostUSD: cost}, serr
}

func toSchemaVerdict(v *agent.ReviewVerdict) schema.ReviewVerdict {
	sv := schema.ReviewVerdict{Verdict: schema.DecisionChangesRequested, Checklist: []schema.ChecklistItem{}}
	if v.Approved() {
		sv.Verdict = schema.DecisionApproved
	}
	for i, c := range v.Criteria {
		item := strings.TrimSpace(c.Criterion)
		if item == "" {
			item = fmt.Sprintf("criterion %d", i+1)
		}
		sv.Checklist = append(sv.Checklist, schema.ChecklistItem{Item: item, Met: c.Met, Note: c.Evidence})
	}
	sv.Notes = feedbackText(v)
	return sv
}

// feedbackText renders the summary and issues of a verdict as plain text.
func feedbackText(v *agent.ReviewVerdict) string {
	var b strings.Builder
	b.WriteString(v.Summary)
	for _, is := range v.Issues {
		b.WriteString("\n- [" + string(is.Severity) + "] ")
		if is.File != "" {
			b.WriteString(is.File)
			if is.Line > 0 {
				fmt.Fprintf(&b, ":%d", is.Line)
			}
			b.WriteString(": ")
		}
		b.WriteString(is.Message)
	}
	return b.String()
}

// ---------------------------------------------------------------- Merge

// Merge squash-merges the task's pull request, removes its worktree and
// branch and marks the task done. It requires the task to be in review with an
// approving review of the exact commit that is now on the PR, and refuses when
// checks fail or are pending, or the PR has conflicts. A draft PR is marked
// ready first. If cleanup fails after a successful merge the returned error
// wraps ErrCleanup and the task is nevertheless done.
func (s *Service) Merge(ctx context.Context, task string) (schema.Task, error) {
	release, err := s.claim(task, "merge")
	if err != nil {
		return schema.Task{}, err
	}
	defer release()
	rec, err := s.GetTask(task)
	if err != nil {
		return schema.Task{}, err
	}
	if rec.Task.Status != schema.StatusReview {
		return schema.Task{}, fmt.Errorf("%w: task %s is %s, cannot merge", ErrInvalidTransition, task, rec.Task.Status)
	}
	if s.gh == nil {
		return schema.Task{}, ErrNoGitHub
	}
	if rec.Task.PRUrl == "" {
		return schema.Task{}, fmt.Errorf("%w: %s", ErrNoPR, task)
	}
	if len(rec.Reviews) == 0 || rec.Reviews[len(rec.Reviews)-1].Verdict != agent.VerdictApprove {
		return schema.Task{}, fmt.Errorf("%w: %s", ErrNotApproved, task)
	}
	reviewed := rec.Reviews[len(rec.Reviews)-1].SHA
	repo, number, err := integrations.ParsePRURL(rec.Task.PRUrl)
	if err != nil {
		return schema.Task{}, err
	}

	ctx, cancel := s.joinCtx(ctx)
	defer cancel()
	st, err := s.gh.CheckPRStatus(ctx, repo, number)
	if err != nil {
		return schema.Task{}, err
	}
	if !st.Merged {
		switch {
		case st.State == "closed":
			return schema.Task{}, fmt.Errorf("%w: %s", ErrPRClosed, rec.Task.PRUrl)
		case st.Checks == integrations.ChecksFailure:
			return schema.Task{}, fmt.Errorf("%w: %s", ErrChecksFailing, strings.Join(st.ChecksFailed, ", "))
		case st.Checks == integrations.ChecksPending:
			return schema.Task{}, fmt.Errorf("%w: %s", ErrChecksPending, rec.Task.PRUrl)
		case st.Mergeable != nil && !*st.Mergeable:
			return schema.Task{}, fmt.Errorf("%w: %s", ErrConflicts, rec.Task.PRUrl)
		case st.HeadSHA != reviewed:
			return schema.Task{}, fmt.Errorf("%w: reviewed %.12s, PR is at %.12s", ErrStaleReview, reviewed, st.HeadSHA)
		}
		if st.Draft {
			if err := s.gh.MarkPRReady(ctx, repo, number); err != nil {
				return schema.Task{}, fmt.Errorf("service: mark ready: %w", err)
			}
		}
		if _, err := s.gh.SquashMerge(ctx, repo, number, integrations.MergeOptions{
			CommitTitle: fmt.Sprintf("%s (#%d)", rec.Task.Title, number), SHA: reviewed,
		}); err != nil {
			return schema.Task{}, err
		}
	}
	done, err := s.finalize(ctx, task, schema.RoleLead, "merged "+rec.Task.PRUrl)
	return done.Task, err
}
