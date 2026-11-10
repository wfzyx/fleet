// Package service is the fleet engine. It owns the spaces, the task
// lifecycle state machine (queued -> working -> review -> done/closed), spend
// tracking and the JSON event log, and exposes them over a Unix-socket
// JSON-RPC server so the TUI, the CLI and external skills share one state.
package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/git"
	"fleet/pkg/integrations"
	"fleet/pkg/pi"
	"fleet/pkg/schema"
)

// Sentinel errors. Use errors.Is; they survive the RPC boundary.
var (
	ErrNoSuchSpace       = errors.New("service: no such space")
	ErrSpaceExists       = errors.New("service: space already exists")
	ErrSpaceInUse        = errors.New("service: space has active tasks")
	ErrNoSuchTask        = errors.New("service: no such task")
	ErrTaskExists        = errors.New("service: task already exists")
	ErrInvalidTransition = errors.New("service: invalid state transition")
	ErrBusy              = errors.New("service: task is busy")
	ErrNoProject         = errors.New("service: no project for task")
	ErrNoCriteria        = errors.New("service: task has no acceptance criteria")
	ErrEmptyDiff         = errors.New("service: nothing to review: empty diff")
	ErrNotApproved       = errors.New("service: task has no approving review")
	ErrStaleReview       = errors.New("service: branch changed since the approving review")
	ErrNoPR              = errors.New("service: task has no pull request")
	ErrUseMerge          = errors.New("service: task has a pull request; use merge")
	ErrNoGitHub          = errors.New("service: github is not configured")
	ErrChecksFailing     = errors.New("service: pull request checks are failing")
	ErrChecksPending     = errors.New("service: pull request checks are still running")
	ErrConflicts         = errors.New("service: pull request has merge conflicts")
	ErrPRClosed          = errors.New("service: pull request is closed without being merged")
	ErrCleanup           = errors.New("service: cleanup failed")
	ErrShutdown          = errors.New("service: shut down")
	ErrInvalidArgument   = errors.New("service: invalid argument")
)

// GitHubAPI is the subset of the GitHub client the service uses.
// *integrations.GitHub implements it.
type GitHubAPI interface {
	ListIssues(ctx context.Context, repo string, opts integrations.IssueListOptions) ([]integrations.Issue, error)
	CreateDraftPR(ctx context.Context, repo string, in integrations.NewPR) (*integrations.PullRequest, error)
	MarkPRReady(ctx context.Context, repo string, number int) error
	SquashMerge(ctx context.Context, repo string, number int, opts integrations.MergeOptions) (*integrations.MergeResult, error)
	CheckPRStatus(ctx context.Context, repo string, number int) (*integrations.PRStatus, error)
}

// JiraAPI is the subset of the Jira client the service uses.
// *integrations.Jira implements it.
type JiraAPI interface {
	TransitionIssue(ctx context.Context, key, target string) error
}

// Worktrees manages task worktrees. *git.Manager implements it.
type Worktrees interface {
	Create(ctx context.Context, o git.CreateOptions) (*git.Worktree, error)
	Diff(ctx context.Context, path, base string) (string, error)
	Head(ctx context.Context, path string) (string, error)
	Remove(ctx context.Context, w *git.Worktree, deleteBranch bool) error
	Push(ctx context.Context, path, branch string) error
	RemoteRepo(ctx context.Context, repo string) (string, error)
}

// Runner launches Pi agents. The default implementation runs the real Pi
// binary through package pi.
type Runner interface {
	RunWorker(ctx context.Context, o pi.WorkerOptions) (*pi.RunResult, error)
	RunReviewer(ctx context.Context, o pi.ReviewerOptions) (*agent.ReviewVerdict, *pi.RunResult, error)
}

// Compile-time checks that the real implementations satisfy the interfaces.
var (
	_ GitHubAPI = (*integrations.GitHub)(nil)
	_ JiraAPI   = (*integrations.Jira)(nil)
	_ Worktrees = (*git.Manager)(nil)
	_ Runner    = piRunner{}
)

type piRunner struct{}

func (piRunner) RunWorker(ctx context.Context, o pi.WorkerOptions) (*pi.RunResult, error) {
	return pi.RunWorker(ctx, o)
}

func (piRunner) RunReviewer(ctx context.Context, o pi.ReviewerOptions) (*agent.ReviewVerdict, *pi.RunResult, error) {
	return pi.RunReviewer(ctx, o)
}

// Options configures New. Every field is optional.
type Options struct {
	// Paths defaults to config.NewPaths().
	Paths config.Paths
	// GitHub enables draft-PR creation and merging; nil disables them.
	GitHub GitHubAPI
	// Jira enables ticket transitions for tasks with a Jira key; nil disables them.
	Jira JiraAPI
	// Worktrees defaults to a *git.Manager on Paths.
	Worktrees Worktrees
	// Runner defaults to the real Pi runner.
	Runner Runner
	// Tracker defaults to a new pi.Tracker restored from the ledger file.
	Tracker *pi.Tracker
	// PiBinary, PiEnv and PiExtensions are passed to every Pi invocation.
	PiBinary     string
	PiEnv        []string
	PiExtensions []string
	// StartLead launches lead agents for Chat; it defaults to the real Pi binary.
	StartLead LeadStarter
	// Now defaults to time.Now.
	Now func() time.Time
}

// LogEntry is one line of a task's log.
type LogEntry struct {
	Time   time.Time        `json:"time"`
	Role   schema.AgentRole `json:"role"`
	Status schema.Status    `json:"status"`
	Note   string           `json:"note"`
}

// ReviewRecord is a stored review outcome.
type ReviewRecord struct {
	Time     time.Time               `json:"time"`
	Verdict  agent.Verdict           `json:"verdict"`
	Summary  string                  `json:"summary"`
	Issues   []agent.Issue           `json:"issues,omitempty"`
	Criteria []agent.CriterionResult `json:"criteria,omitempty"`
	SHA      string                  `json:"sha,omitempty"` // branch head that was reviewed
	CostUSD  float64                 `json:"cost_usd,omitempty"`
}

// TaskRecord is a task plus the engine's bookkeeping about it.
type TaskRecord struct {
	Task       schema.Task    `json:"task"`
	Repo       string         `json:"repo,omitempty"`        // main checkout the worktree belongs to
	BaseBranch string         `json:"base_branch,omitempty"` // PR target branch
	BaseRef    string         `json:"base_ref,omitempty"`    // ref the diff is computed against
	GitHubRepo string         `json:"github_repo,omitempty"` // owner/name of the PR's repository
	Blocked    bool           `json:"blocked,omitempty"`
	Feedback   string         `json:"feedback,omitempty"` // reviewer findings for the next worker run
	Reviews    []ReviewRecord `json:"reviews,omitempty"`
	Log        []LogEntry     `json:"log"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

func (r *TaskRecord) clone() TaskRecord {
	c := *r
	c.Task.AcceptanceCriteria = slices.Clone(r.Task.AcceptanceCriteria)
	c.Reviews = slices.Clone(r.Reviews)
	c.Log = slices.Clone(r.Log)
	return c
}

// maxLogEntries bounds a task's log; the oldest entries are dropped.
const maxLogEntries = 1000

type tasksFile struct {
	Version int          `json:"version"`
	Tasks   []TaskRecord `json:"tasks"`
}

const tasksFileVersion = 1

// Service is the fleet engine. It is safe for concurrent use.
type Service struct {
	opts  Options
	paths config.Paths
	now   func() time.Time

	wt     Worktrees
	runner Runner
	gh     GitHubAPI
	jira   JiraAPI

	tracker *pi.Tracker

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex // guards cfg, tasks, busy, runs, closed
	cfg    config.Config
	tasks  map[string]*TaskRecord
	busy   map[string]string // task -> operation in flight
	runs   map[string]*workerRun
	closed bool

	leadMu sync.Mutex // guards leads (each lead has its own locks)
	leads  map[string]*lead

	evMu    sync.Mutex // guards subs and serialises event-log appends
	subs    map[int]chan schema.AgentEvent
	nextSub int
}

// New loads (or initialises) the fleet state under the XDG directories.
func New(opts Options) (*Service, error) {
	paths := opts.Paths
	if paths.DataDir == "" {
		var err error
		if paths, err = config.NewPaths(); err != nil {
			return nil, err
		}
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		return nil, err
	}
	s := &Service{
		opts: opts, paths: paths, now: opts.Now, wt: opts.Worktrees, runner: opts.Runner,
		gh: opts.GitHub, jira: opts.Jira, tracker: opts.Tracker, cfg: cfg,
		tasks: map[string]*TaskRecord{}, busy: map[string]string{}, runs: map[string]*workerRun{},
		subs: map[int]chan schema.AgentEvent{}, leads: map[string]*lead{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.wt == nil {
		s.wt = git.NewManager(paths)
	}
	if s.runner == nil {
		s.runner = piRunner{}
	}
	if s.tracker == nil {
		s.tracker = pi.NewTracker(pi.Budgets{})
		if err := s.tracker.Load(s.ledgerPath()); err != nil {
			return nil, err
		}
	}
	if err := s.loadTasks(); err != nil {
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.mu.Lock()
	s.refreshBudgetsLocked()
	s.mu.Unlock()
	return s, nil
}

// Paths returns the resolved fleet directories.
func (s *Service) Paths() config.Paths { return s.paths }

// Shutdown stops all workers, waits for them to exit (or ctx to end) and
// flushes the spend ledger. Task state is left as is, so tasks that were
// working can be resumed with Spawn after a restart.
func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.stopLeads("")

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	var werr error
	select {
	case <-done:
	case <-ctx.Done():
		werr = fmt.Errorf("service: shutdown: %w", ctx.Err())
	}
	s.evMu.Lock()
	for id, ch := range s.subs {
		close(ch)
		delete(s.subs, id)
	}
	s.evMu.Unlock()
	return errors.Join(werr, s.tracker.Save(s.ledgerPath()))
}

func (s *Service) tasksPath() string  { return filepath.Join(s.paths.DataDir, "state", "tasks.json") }
func (s *Service) eventsPath() string { return filepath.Join(s.paths.DataDir, "events.jsonl") }
func (s *Service) ledgerPath() string { return filepath.Join(s.paths.DataDir, "ledger.json") }

// ---------------------------------------------------------------- persistence

func (s *Service) loadTasks() error {
	data, err := os.ReadFile(s.tasksPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: read tasks: %w", err)
	}
	var f tasksFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("service: parse %s: %w", s.tasksPath(), err)
	}
	if f.Version != tasksFileVersion {
		return fmt.Errorf("service: %s has unsupported version %d", s.tasksPath(), f.Version)
	}
	for i := range f.Tasks {
		r := f.Tasks[i]
		if err := r.Task.Validate(); err != nil {
			return fmt.Errorf("service: stored task %q: %w", r.Task.ID, err)
		}
		if _, dup := s.tasks[r.Task.ID]; dup {
			return fmt.Errorf("service: duplicate stored task %q", r.Task.ID)
		}
		s.tasks[r.Task.ID] = &r
	}
	return nil
}

func (s *Service) saveTasksLocked() error {
	f := tasksFile{Version: tasksFileVersion, Tasks: make([]TaskRecord, 0, len(s.tasks))}
	for _, r := range s.tasks {
		f.Tasks = append(f.Tasks, *r)
	}
	sort.Slice(f.Tasks, func(i, j int) bool {
		a, b := f.Tasks[i], f.Tasks[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.Task.ID < b.Task.ID
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("service: encode tasks: %w", err)
	}
	return config.WriteFileAtomic(s.tasksPath(), append(data, '\n'), 0o600)
}

// saveConfigLocked persists cfg; on failure it restores old.
func (s *Service) saveConfigLocked(old config.Config) error {
	if err := config.Save(s.paths.ConfigFile(), s.cfg); err != nil {
		s.cfg = old
		s.refreshBudgetsLocked()
		return err
	}
	return nil
}

func cloneConfig(c config.Config) config.Config {
	out := c
	out.Spaces = make([]config.Space, len(c.Spaces))
	for i, sp := range c.Spaces {
		out.Spaces[i] = sp
		out.Spaces[i].Projects = slices.Clone(sp.Projects)
	}
	return out
}

// ---------------------------------------------------------------- spaces

// ListSpaces returns all spaces sorted by ID.
func (s *Service) ListSpaces() []config.Space {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := cloneConfig(s.cfg).Spaces
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetSpace returns one space.
func (s *Service) GetSpace(id string) (config.Space, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.cfg.Space(id)
	if !ok {
		return config.Space{}, fmt.Errorf("%w: %q", ErrNoSuchSpace, id)
	}
	c := *sp
	c.Projects = slices.Clone(sp.Projects)
	return c, nil
}

func checkProjectDir(p config.Project) error {
	if fi, err := os.Stat(p.Path); err != nil || !fi.IsDir() {
		return fmt.Errorf("%w: project %q path %q is not a directory", ErrInvalidArgument, p.ID, p.Path)
	}
	return nil
}

// AddSpace registers a new space. The name defaults to the ID.
func (s *Service) AddSpace(sp config.Space) error {
	if sp.Name == "" {
		sp.Name = sp.ID
	}
	if sp.Projects == nil {
		sp.Projects = []config.Project{}
	}
	for i := range sp.Projects {
		if sp.Projects[i].Name == "" {
			sp.Projects[i].Name = sp.Projects[i].ID
		}
		if err := checkProjectDir(sp.Projects[i]); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrShutdown
	}
	if _, ok := s.cfg.Space(sp.ID); ok {
		return fmt.Errorf("%w: %q", ErrSpaceExists, sp.ID)
	}
	old := cloneConfig(s.cfg)
	s.cfg.Spaces = append(s.cfg.Spaces, sp)
	if err := s.cfg.Validate(); err != nil {
		s.cfg = old
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s.refreshBudgetsLocked()
	return s.saveConfigLocked(old)
}

// AddProject adds a project to an existing space.
func (s *Service) AddProject(spaceID string, p config.Project) error {
	if p.Name == "" {
		p.Name = p.ID
	}
	if err := checkProjectDir(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrShutdown
	}
	sp, ok := s.cfg.Space(spaceID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchSpace, spaceID)
	}
	old := cloneConfig(s.cfg)
	sp.Projects = append(sp.Projects, p)
	if err := s.cfg.Validate(); err != nil {
		s.cfg = old
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	return s.saveConfigLocked(old)
}

// RemoveSpace deletes a space and its task records. It fails with
// ErrSpaceInUse while tasks are active, unless force is set, in which case
// they are closed first.
func (s *Service) RemoveSpace(ctx context.Context, id string, force bool) error {
	s.mu.Lock()
	if _, ok := s.cfg.Space(id); !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNoSuchSpace, id)
	}
	var active []string
	for _, r := range s.tasks {
		if r.Task.SpaceID == id && !r.Task.Status.Terminal() {
			active = append(active, r.Task.ID)
		}
	}
	s.mu.Unlock()
	sort.Strings(active)
	if len(active) > 0 && !force {
		return fmt.Errorf("%w: %v", ErrSpaceInUse, active)
	}
	for _, t := range active {
		if _, err := s.CloseTask(ctx, t, "space removed"); err != nil && !errors.Is(err, ErrInvalidTransition) {
			return fmt.Errorf("close task %s: %w", t, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrShutdown
	}
	old := cloneConfig(s.cfg)
	oldTasks := s.tasks
	s.cfg.Spaces = slices.DeleteFunc(s.cfg.Spaces, func(sp config.Space) bool { return sp.ID == id })
	s.tasks = make(map[string]*TaskRecord, len(oldTasks))
	for k, r := range oldTasks {
		if r.Task.SpaceID != id {
			s.tasks[k] = r
		}
	}
	if err := s.saveConfigLocked(old); err != nil {
		s.tasks = oldTasks
		return err
	}
	if err := s.saveTasksLocked(); err != nil {
		s.tasks = oldTasks
		s.cfg = old
		_ = config.Save(s.paths.ConfigFile(), old) // best effort: keep config and tasks consistent
		return err
	}
	s.refreshBudgetsLocked()
	go s.stopLeads(id)
	return nil
}

// ---------------------------------------------------------------- budgets

// BudgetUpdate changes budgets; nil fields are left unchanged. Zero means unlimited.
type BudgetUpdate struct {
	MaxUSD     *float64 `json:"max_usd,omitempty"`      // total spend of the space
	TaskMaxUSD *float64 `json:"task_max_usd,omitempty"` // spend of each task in the space
}

func validBudget(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// SetSpaceBudget updates the budgets of a space.
func (s *Service) SetSpaceBudget(spaceID string, u BudgetUpdate) error {
	for _, v := range []*float64{u.MaxUSD, u.TaskMaxUSD} {
		if v != nil && !validBudget(*v) {
			return fmt.Errorf("%w: budget %v must be finite and non-negative", ErrInvalidArgument, *v)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.cfg.Space(spaceID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchSpace, spaceID)
	}
	old := cloneConfig(s.cfg)
	if u.MaxUSD != nil {
		sp.MaxBudgetUSD = *u.MaxUSD
	}
	if u.TaskMaxUSD != nil {
		sp.TaskBudgetUSD = *u.TaskMaxUSD
	}
	s.refreshBudgetsLocked()
	return s.saveConfigLocked(old)
}

// SetFleetBudget sets the fleet-wide spend limit (0 = unlimited).
func (s *Service) SetFleetBudget(maxUSD float64) error {
	if !validBudget(maxUSD) {
		return fmt.Errorf("%w: budget %v must be finite and non-negative", ErrInvalidArgument, maxUSD)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := cloneConfig(s.cfg)
	s.cfg.Budget.MaxBudgetUSD = maxUSD
	return s.saveConfigLocked(old)
}

// refreshBudgetsLocked rebuilds the tracker's limits from the config.
func (s *Service) refreshBudgetsLocked() {
	b := pi.Budgets{SpaceOverrides: map[string]float64{}, TaskOverrides: map[string]float64{}}
	for _, sp := range s.cfg.Spaces {
		if sp.MaxBudgetUSD > 0 {
			b.SpaceOverrides[sp.ID] = sp.MaxBudgetUSD
		}
		if sp.TaskBudgetUSD > 0 {
			for _, r := range s.tasks {
				if r.Task.SpaceID == sp.ID {
					b.TaskOverrides[sp.ID+"/"+r.Task.ID] = sp.TaskBudgetUSD
				}
			}
		}
	}
	s.tracker.SetBudgets(b)
}

// checkBudget returns an error wrapping pi.ErrBudgetExceeded when the task,
// its space or the whole fleet has reached its limit.
func (s *Service) checkBudget(space, task string) error {
	if err := s.tracker.Enforce(space, task); err != nil {
		return err
	}
	s.mu.Lock()
	b := s.cfg.Budget
	s.mu.Unlock()
	if b.Exceeded() {
		return fmt.Errorf("%w: fleet spent $%.4f of $%.4f", pi.ErrBudgetExceeded, b.CurrentSpendUSD, b.MaxBudgetUSD)
	}
	return nil
}

// syncSpend copies tracker totals into the task record and the fleet budget,
// persists the ledger, and returns how much the task's spend grew.
func (s *Service) syncSpend(space, task string) (float64, error) {
	spent := s.tracker.TaskUsage(space, task).Cost.Total
	s.mu.Lock()
	var total float64
	for _, sp := range s.cfg.Spaces {
		total += s.tracker.SpaceUsage(sp.ID).Cost.Total
	}
	var delta float64
	var errs []error
	if r, ok := s.tasks[task]; ok && r.Task.SpaceID == space && r.Task.SpendUSD != spent {
		delta = spent - r.Task.SpendUSD
		prev := r.Task.SpendUSD
		r.Task.SpendUSD = spent
		if err := s.saveTasksLocked(); err != nil {
			r.Task.SpendUSD = prev
			errs = append(errs, err)
		}
	}
	if s.cfg.Budget.CurrentSpendUSD != total {
		old := cloneConfig(s.cfg)
		s.cfg.Budget.CurrentSpendUSD = total
		if err := s.saveConfigLocked(old); err != nil {
			errs = append(errs, err)
		}
	}
	s.mu.Unlock()
	errs = append(errs, s.tracker.Save(s.ledgerPath()))
	return delta, errors.Join(errs...)
}

// CostReport is the spend of a space or task against its budgets.
type CostReport struct {
	Space    string            `json:"space,omitempty"`
	Task     string            `json:"task,omitempty"`
	Usage    pi.Usage          `json:"usage"`
	Models   []pi.ModelUsage   `json:"models,omitempty"`
	Statuses []pi.BudgetStatus `json:"statuses,omitempty"`
	Fleet    config.Budget     `json:"fleet"`
}

// Cost re-reads the Pi sessions of the task (all of the space's tasks when
// task is empty), updates the stored spend and reports it against the budgets.
func (s *Service) Cost(space, task string) (CostReport, error) {
	if _, err := s.GetSpace(space); err != nil {
		return CostReport{}, err
	}
	if task != "" {
		if _, err := s.taskInSpace(space, task); err != nil {
			return CostReport{}, err
		}
	}
	for _, id := range s.taskIDs(space, task) {
		for _, role := range []agent.Role{agent.RoleWorker, agent.RoleReviewer} {
			dir, err := agent.SessionDir(role, space, id)
			if err != nil {
				return CostReport{}, err
			}
			if _, err := s.tracker.IngestDir(space, id, role, dir); err != nil {
				return CostReport{}, err
			}
		}
		if _, err := s.syncSpend(space, id); err != nil {
			return CostReport{}, err
		}
	}
	rep := CostReport{Space: space, Task: task, Statuses: s.tracker.Check(space, task), Models: s.tracker.ModelUsage(space, task)}
	if task != "" {
		rep.Usage = s.tracker.TaskUsage(space, task)
	} else {
		rep.Usage = s.tracker.SpaceUsage(space)
	}
	s.mu.Lock()
	rep.Fleet = s.cfg.Budget
	s.mu.Unlock()
	return rep, nil
}

func (s *Service) taskIDs(space, task string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, r := range s.tasks {
		if r.Task.SpaceID == space && (task == "" || id == task) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// ---------------------------------------------------------------- events

// Subscribe returns a channel of events emitted after the call and a
// function that cancels the subscription. Events are dropped for a
// subscriber whose buffer is full; the channel is closed on cancel or Shutdown.
func (s *Service) Subscribe(buffer int) (<-chan schema.AgentEvent, func()) {
	if buffer < 1 {
		buffer = 64
	}
	ch := make(chan schema.AgentEvent, buffer)
	s.evMu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	s.evMu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.evMu.Lock()
			defer s.evMu.Unlock()
			if c, ok := s.subs[id]; ok {
				close(c)
				delete(s.subs, id)
			}
		})
	}
}

// emit validates e, appends it to the JSONL event log and notifies
// subscribers. Failures are logged, never fatal: state has already changed.
func (s *Service) emit(e schema.AgentEvent) {
	e.Timestamp = s.now().UTC()
	line, err := schema.MarshalEvent(e)
	if err != nil {
		slog.Error("fleet: invalid event", "type", e.Type, "task", e.TaskID, "err", err)
		return
	}
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if err := appendLine(s.eventsPath(), line); err != nil {
		slog.Error("fleet: write event log", "err", err)
	}
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop rather than block the engine
		}
	}
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	return errors.Join(werr, f.Close())
}

// Events returns the last limit events from the event log (all when limit <= 0).
// Lines that do not decode are skipped.
func (s *Service) Events(limit int) ([]schema.AgentEvent, error) {
	s.evMu.Lock()
	f, err := os.Open(s.eventsPath())
	s.evMu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return []schema.AgentEvent{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []schema.AgentEvent{}
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if e, err := schema.UnmarshalEvent(line); err == nil {
				out = append(out, e)
			}
		}
		if rerr != nil {
			break
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
