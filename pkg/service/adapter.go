package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/schema"
	"fleet/pkg/tui"
)

// API is the fleet surface the TUI adapter needs. *Client implements it over
// the Unix socket; Local implements it on an in-process *Service.
type API interface {
	Spaces(ctx context.Context) ([]config.Space, error)
	Tasks(ctx context.Context, f TaskFilter) ([]schema.Task, error)
	GetTask(ctx context.Context, task string) (TaskRecord, error)
	Diff(ctx context.Context, task string) (string, error)
	Delegate(ctx context.Context, p DelegateParams) (schema.Task, error)
	Spawn(ctx context.Context, task, branch string) (schema.Task, error)
	Report(ctx context.Context, task, status, note string) (schema.Task, error)
	Review(ctx context.Context, task string) (ReviewResult, error)
	Merge(ctx context.Context, task string) (schema.Task, error)
	CloseTask(ctx context.Context, task, note string) (schema.Task, error)
	Chat(ctx context.Context, space, text string) (string, error)
	LeadStatuses(ctx context.Context) (map[string]string, error)
}

var (
	_ API = (*Client)(nil)
	_ API = Local{}
)

// Local adapts an in-process *Service to API.
type Local struct{ S *Service }

// Spaces implements API.
func (l Local) Spaces(context.Context) ([]config.Space, error) { return l.S.ListSpaces(), nil }

// Tasks implements API.
func (l Local) Tasks(_ context.Context, f TaskFilter) ([]schema.Task, error) {
	return l.S.ListTasks(f), nil
}

// GetTask implements API.
func (l Local) GetTask(_ context.Context, id string) (TaskRecord, error) { return l.S.GetTask(id) }

// Diff implements API.
func (l Local) Diff(ctx context.Context, id string) (string, error) { return l.S.Diff(ctx, id) }

// Delegate implements API.
func (l Local) Delegate(_ context.Context, p DelegateParams) (schema.Task, error) {
	var opts []DelegateOption
	if p.Title != "" {
		opts = append(opts, WithTitle(p.Title))
	}
	if p.Project != "" {
		opts = append(opts, WithProject(p.Project))
	}
	if p.JiraKey != "" {
		opts = append(opts, WithJiraKey(p.JiraKey))
	}
	return l.S.Delegate(p.Space, p.Task, p.Brief, p.Criteria, opts...)
}

// Spawn implements API.
func (l Local) Spawn(ctx context.Context, task, branch string) (schema.Task, error) {
	return l.S.Spawn(ctx, task, branch)
}

// Report implements API.
func (l Local) Report(ctx context.Context, task, status, note string) (schema.Task, error) {
	return l.S.Report(ctx, task, status, note)
}

// Review implements API.
func (l Local) Review(ctx context.Context, task string) (ReviewResult, error) {
	return l.S.Review(ctx, task)
}

// Merge implements API.
func (l Local) Merge(ctx context.Context, task string) (schema.Task, error) {
	return l.S.Merge(ctx, task)
}

// CloseTask implements API.
func (l Local) CloseTask(ctx context.Context, task, note string) (schema.Task, error) {
	return l.S.CloseTask(ctx, task, note)
}

// Chat implements API.
func (l Local) Chat(ctx context.Context, space, text string) (string, error) {
	return l.S.Chat(ctx, space, text)
}

// LeadStatuses implements API.
func (l Local) LeadStatuses(context.Context) (map[string]string, error) {
	return l.S.LeadStatuses(), nil
}

// Adapter implements tui.Backend on top of an API. Besides the Backend
// methods it exposes the underlying fleet operations under their service names.
type Adapter struct {
	api API
}

var _ tui.Backend = (*Adapter)(nil)

// NewAdapter returns a tui.Backend backed by api (a *Client or Local).
func NewAdapter(api API) *Adapter { return &Adapter{api: api} }

// NewLocalAdapter returns a tui.Backend backed by an in-process service.
func NewLocalAdapter(s *Service) *Adapter { return NewAdapter(Local{S: s}) }

// ---------------------------------------------------------------- tui.Backend

// Spaces implements tui.Backend: spaces with their spend, budget, lead state
// and number of active tasks.
func (a *Adapter) Spaces(ctx context.Context) ([]tui.SpaceInfo, error) {
	spaces, err := a.api.Spaces(ctx)
	if err != nil {
		return nil, err
	}
	tasks, err := a.api.Tasks(ctx, TaskFilter{})
	if err != nil {
		return nil, err
	}
	leads, err := a.api.LeadStatuses(ctx)
	if err != nil {
		leads = nil // lead state is cosmetic: show offline rather than fail the sidebar
	}
	type agg struct {
		spend  float64
		active int
	}
	sums := map[string]*agg{}
	for _, t := range tasks {
		g := sums[t.SpaceID]
		if g == nil {
			g = &agg{}
			sums[t.SpaceID] = g
		}
		g.spend += t.SpendUSD
		if !t.Status.Terminal() {
			g.active++
		}
	}
	out := make([]tui.SpaceInfo, 0, len(spaces))
	for _, sp := range spaces {
		info := tui.SpaceInfo{ID: sp.ID, Name: sp.Name, BudgetUSD: sp.MaxBudgetUSD, Lead: leadStatus(leads[sp.ID])}
		if g := sums[sp.ID]; g != nil {
			info.SpendUSD, info.ActiveTasks = g.spend, g.active
		}
		if info.Name == "" {
			info.Name = sp.ID
		}
		out = append(out, info)
	}
	return out, nil
}

func leadStatus(s string) tui.LeadStatus {
	switch s {
	case LeadIdle:
		return tui.LeadIdle
	case LeadWorking:
		return tui.LeadWorking
	}
	return tui.LeadOffline
}

// Tasks implements tui.Backend.
func (a *Adapter) Tasks(ctx context.Context, spaceID string) ([]schema.Task, error) {
	return a.api.Tasks(ctx, TaskFilter{Space: spaceID})
}

// Detail implements tui.Backend: diff, worker log and latest verdict.
func (a *Adapter) Detail(ctx context.Context, task schema.Task) (tui.TaskDetail, error) {
	rec, err := a.api.GetTask(ctx, task.ID)
	if err != nil {
		return tui.TaskDetail{}, err
	}
	var d tui.TaskDetail
	for _, e := range rec.Log {
		d.Logs = append(d.Logs, formatLog(e))
	}
	if n := len(rec.Reviews); n > 0 {
		v := reviewToSchema(rec.Reviews[n-1])
		d.Verdict = &v
	}
	diff, err := a.api.Diff(ctx, task.ID)
	if err != nil {
		// The task data is still useful without the diff.
		d.Diff = "diff unavailable: " + err.Error()
	} else {
		d.Diff = diff
	}
	return d, nil
}

func formatLog(e LogEntry) string {
	return fmt.Sprintf("%s %-8s %s", e.Time.Local().Format("15:04:05"), e.Role, e.Note)
}

// reviewToSchema converts a stored review into the TUI's checklist form.
func reviewToSchema(r ReviewRecord) schema.ReviewVerdict {
	v := schema.ReviewVerdict{Verdict: schema.DecisionChangesRequested, Checklist: []schema.ChecklistItem{}}
	if r.Verdict == agent.VerdictApprove {
		v.Verdict = schema.DecisionApproved
	}
	for i, c := range r.Criteria {
		item := strings.TrimSpace(c.Criterion)
		if item == "" {
			item = fmt.Sprintf("criterion %d", i+1)
		}
		v.Checklist = append(v.Checklist, schema.ChecklistItem{Item: item, Met: c.Met, Note: c.Evidence})
	}
	v.Notes = r.Summary
	for _, is := range r.Issues {
		loc := is.File
		if loc != "" && is.Line > 0 {
			loc += ":" + strconv.Itoa(is.Line)
		}
		line := fmt.Sprintf("[%s] %s", is.Severity, is.Message)
		if loc != "" {
			line = loc + " " + line
		}
		v.Notes += "\n- " + line
	}
	v.Notes = strings.TrimSpace(v.Notes)
	// Keep the verdict self-consistent for the TUI: changes without any
	// explanation cannot be shown, so fall back to the raw verdict name.
	if v.Verdict == schema.DecisionChangesRequested && v.Notes == "" {
		v.Notes = string(r.Verdict)
	}
	return v
}

// Chat implements tui.Backend by sending text to the space's lead.
func (a *Adapter) Chat(ctx context.Context, spaceID, text string) (string, error) {
	return a.SendLeadMessage(ctx, spaceID, text)
}

// CreateTask implements tui.Backend: it registers a queued task whose ID is
// derived from the title.
func (a *Adapter) CreateTask(ctx context.Context, spaceID, title, brief string) (schema.Task, error) {
	title = strings.TrimSpace(title)
	if strings.TrimSpace(brief) == "" {
		brief = title
	}
	base := slug(title)
	for i := 1; i <= 50; i++ {
		id := base
		if i > 1 {
			id = base + "-" + strconv.Itoa(i)
		}
		t, err := a.api.Delegate(ctx, DelegateParams{Space: spaceID, Task: id, Brief: brief, Title: title})
		if errors.Is(err, ErrTaskExists) {
			continue
		}
		return t, err
	}
	return schema.Task{}, fmt.Errorf("%w: no free task id for %q", ErrTaskExists, title)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug turns a title into a short, valid task ID.
func slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// Delegate implements tui.Backend: it starts a worker for a queued task.
func (a *Adapter) Delegate(ctx context.Context, task schema.Task) error {
	_, err := a.api.Spawn(ctx, task.ID, "")
	return err
}

// RequestReview implements tui.Backend: it moves a working task to review and,
// when it has acceptance criteria, runs the reviewer on it right away.
func (a *Adapter) RequestReview(ctx context.Context, task schema.Task) error {
	if _, err := a.api.Report(ctx, task.ID, string(schema.StatusReview), "review requested from the cockpit"); err != nil {
		return err
	}
	if len(task.AcceptanceCriteria) == 0 {
		return nil // nothing for the reviewer to judge; the user decides
	}
	if _, err := a.api.Review(ctx, task.ID); err != nil && !errors.Is(err, ErrEmptyDiff) && !errors.Is(err, ErrNoCriteria) {
		return err
	}
	return nil
}

// Approve implements tui.Backend: it merges the task's pull request, or marks
// a task without a pull request done.
func (a *Adapter) Approve(ctx context.Context, task schema.Task) error {
	rec, err := a.api.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}
	if rec.Task.PRUrl != "" {
		_, err = a.api.Merge(ctx, task.ID)
		return err
	}
	_, err = a.api.Report(ctx, task.ID, string(schema.StatusDone), "approved from the cockpit")
	return err
}

// ---------------------------------------------------------------- service operations

// ListSpaces returns the registered spaces.
func (a *Adapter) ListSpaces(ctx context.Context) ([]config.Space, error) {
	return a.api.Spaces(ctx)
}

// ListTasks returns the tasks matching f.
func (a *Adapter) ListTasks(ctx context.Context, f TaskFilter) ([]schema.Task, error) {
	return a.api.Tasks(ctx, f)
}

// GetTask returns a task with its log and reviews.
func (a *Adapter) GetTask(ctx context.Context, id string) (TaskRecord, error) {
	return a.api.GetTask(ctx, id)
}

// DelegateTask registers a queued task.
func (a *Adapter) DelegateTask(ctx context.Context, p DelegateParams) (schema.Task, error) {
	return a.api.Delegate(ctx, p)
}

// Spawn starts (or relaunches) the worker of a task.
func (a *Adapter) Spawn(ctx context.Context, task, branch string) (schema.Task, error) {
	return a.api.Spawn(ctx, task, branch)
}

// Report reports a task status or progress note.
func (a *Adapter) Report(ctx context.Context, task, status, note string) (schema.Task, error) {
	return a.api.Report(ctx, task, status, note)
}

// Review runs the reviewer on a task in review.
func (a *Adapter) Review(ctx context.Context, task string) (ReviewResult, error) {
	return a.api.Review(ctx, task)
}

// Merge merges the approved pull request of a task.
func (a *Adapter) Merge(ctx context.Context, task string) (schema.Task, error) {
	return a.api.Merge(ctx, task)
}

// CloseTask abandons a task and cleans up its worktree.
func (a *Adapter) CloseTask(ctx context.Context, task, note string) (schema.Task, error) {
	return a.api.CloseTask(ctx, task, note)
}

// SendLeadMessage sends text to the space's lead and returns its reply.
func (a *Adapter) SendLeadMessage(ctx context.Context, space, text string) (string, error) {
	return a.api.Chat(ctx, space, text)
}
