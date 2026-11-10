package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"fleet/pkg/agent"
)

// ErrBudgetExceeded is returned (wrapped) when spending has reached a limit.
var ErrBudgetExceeded = errors.New("pi: budget exceeded")

// DefaultWarnRatio is the fraction of a limit at which status becomes Warning.
const DefaultWarnRatio = 0.8

// Budgets configures spending limits in USD. A limit <= 0 means unlimited.
type Budgets struct {
	// Space is the default limit for the total spend of a space.
	Space float64 `json:"space,omitempty"`
	// Task is the default limit for the total spend of one task.
	Task float64 `json:"task,omitempty"`
	// SpaceOverrides sets per-space limits, keyed by space.
	SpaceOverrides map[string]float64 `json:"space_overrides,omitempty"`
	// TaskOverrides sets per-task limits, keyed by "space/task".
	TaskOverrides map[string]float64 `json:"task_overrides,omitempty"`
	// WarnRatio overrides DefaultWarnRatio when in (0,1).
	WarnRatio float64 `json:"warn_ratio,omitempty"`
}

func (b Budgets) spaceLimit(space string) float64 {
	if v, ok := b.SpaceOverrides[space]; ok {
		return v
	}
	return b.Space
}

func (b Budgets) taskLimit(space, task string) float64 {
	if v, ok := b.TaskOverrides[space+"/"+task]; ok {
		return v
	}
	return b.Task
}

func (b Budgets) warnRatio() float64 {
	if b.WarnRatio > 0 && b.WarnRatio < 1 {
		return b.WarnRatio
	}
	return DefaultWarnRatio
}

// Scope is the level a budget applies to.
type Scope string

const (
	ScopeTask  Scope = "task"
	ScopeSpace Scope = "space"
)

// BudgetState classifies spending relative to a limit.
type BudgetState string

const (
	BudgetUnlimited BudgetState = "unlimited"
	BudgetOK        BudgetState = "ok"
	BudgetWarning   BudgetState = "warning"
	BudgetExceeded  BudgetState = "exceeded"
)

// BudgetStatus is the result of checking one scope against its limit.
type BudgetStatus struct {
	Scope Scope       `json:"scope"`
	Space string      `json:"space"`
	Task  string      `json:"task,omitempty"`
	Spent float64     `json:"spent"`
	Limit float64     `json:"limit"` // 0 when unlimited
	State BudgetState `json:"state"`
}

// Remaining returns the dollars left (negative when exceeded); +Inf-like
// semantics are not used: unlimited budgets report 0 and State==BudgetUnlimited.
func (s BudgetStatus) Remaining() float64 {
	if s.State == BudgetUnlimited {
		return 0
	}
	return s.Limit - s.Spent
}

func classify(spent, limit, warn float64) BudgetState {
	switch {
	case limit <= 0:
		return BudgetUnlimited
	case spent >= limit:
		return BudgetExceeded
	case spent >= limit*warn:
		return BudgetWarning
	}
	return BudgetOK
}

// Entry is the recorded spend of one session file.
type Entry struct {
	Space string     `json:"space"`
	Task  string     `json:"task,omitempty"` // empty for space-level sessions (the lead)
	Role  agent.Role `json:"role"`
	// Session identifies the session (its file path); re-recording the same
	// session replaces its entry, so repeated ingestion is idempotent.
	Session        string                 `json:"session"`
	Usage          Usage                  `json:"usage"`
	ByModel        map[string]*ModelUsage `json:"by_model,omitempty"`
	Duration       time.Duration          `json:"duration"`
	ActiveDuration time.Duration          `json:"active_duration"`
	UpdatedAt      time.Time              `json:"updated_at"`
}

type entryKey struct{ space, task, session string }

// Tracker accumulates spend per space and task and checks it against Budgets.
// It is safe for concurrent use.
type Tracker struct {
	mu      sync.Mutex
	budgets Budgets
	entries map[entryKey]Entry
	now     func() time.Time
}

// NewTracker returns an empty tracker enforcing b.
func NewTracker(b Budgets) *Tracker {
	return &Tracker{budgets: b, entries: map[entryKey]Entry{}, now: time.Now}
}

// SetBudgets replaces the budgets.
func (t *Tracker) SetBudgets(b Budgets) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.budgets = b
}

// Record stores (or replaces) the spend of one session. task is empty for
// space-level sessions such as the lead's.
func (t *Tracker) Record(space, task string, role agent.Role, st *SessionStats) error {
	if err := agent.ValidateName("space", space); err != nil {
		return err
	}
	if task != "" {
		if err := agent.ValidateName("task", task); err != nil {
			return err
		}
	}
	if st == nil {
		return errors.New("pi: nil session stats")
	}
	session := st.Path
	if session == "" {
		session = st.ID
	}
	if session == "" {
		return errors.New("pi: session has neither path nor id")
	}
	by := make(map[string]*ModelUsage, len(st.ByModel))
	for k, m := range st.ByModel {
		c := *m
		by[k] = &c
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[entryKey{space, task, session}] = Entry{
		Space: space, Task: task, Role: role, Session: session,
		Usage: st.Total, ByModel: by,
		Duration: st.Duration, ActiveDuration: st.ActiveDuration,
		UpdatedAt: t.now().UTC(),
	}
	return nil
}

// IngestDir parses every session below dir and records it. It returns the
// aggregate of the parsed sessions. A missing dir records nothing.
func (t *Tracker) IngestDir(space, task string, role agent.Role, dir string) (*SessionStats, error) {
	sessions, err := ParseSessionDir(dir)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if err := t.Record(space, task, role, s); err != nil {
			return nil, err
		}
	}
	return SumSessions(sessions), nil
}

// TaskUsage returns the cumulative usage of one task.
func (t *Tracker) TaskUsage(space, task string) Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sum(func(k entryKey) bool { return k.space == space && k.task == task })
}

// SpaceUsage returns the cumulative usage of a space, tasks and lead included.
func (t *Tracker) SpaceUsage(space string) Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sum(func(k entryKey) bool { return k.space == space })
}

func (t *Tracker) sum(match func(entryKey) bool) Usage {
	var u Usage
	for k, e := range t.entries {
		if match(k) {
			u.Add(e.Usage)
		}
	}
	return u
}

// ModelUsage returns per-model usage for a space, or for one task when task is
// non-empty, sorted by descending cost.
func (t *Tracker) ModelUsage(space, task string) []ModelUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	agg := &SessionStats{ByModel: map[string]*ModelUsage{}}
	for k, e := range t.entries {
		if k.space != space || (task != "" && k.task != task) {
			continue
		}
		for key, m := range e.ByModel {
			a := agg.ByModel[key]
			if a == nil {
				a = &ModelUsage{Provider: m.Provider, Model: m.Model}
				agg.ByModel[key] = a
			}
			a.Requests += m.Requests
			a.Usage.Add(m.Usage)
		}
	}
	return agg.Models()
}

// Entries returns a copy of all entries sorted by space, task, session.
func (t *Tracker) Entries() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Space != b.Space {
			return a.Space < b.Space
		}
		if a.Task != b.Task {
			return a.Task < b.Task
		}
		return a.Session < b.Session
	})
	return out
}

// Check returns the task status (when task != "") followed by the space status.
func (t *Tracker) Check(space, task string) []BudgetStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	warn := t.budgets.warnRatio()
	var out []BudgetStatus
	if task != "" {
		spent := t.sum(func(k entryKey) bool { return k.space == space && k.task == task }).Cost.Total
		limit := t.budgets.taskLimit(space, task)
		out = append(out, BudgetStatus{Scope: ScopeTask, Space: space, Task: task, Spent: spent, Limit: max(limit, 0), State: classify(spent, limit, warn)})
	}
	spent := t.sum(func(k entryKey) bool { return k.space == space }).Cost.Total
	limit := t.budgets.spaceLimit(space)
	out = append(out, BudgetStatus{Scope: ScopeSpace, Space: space, Spent: spent, Limit: max(limit, 0), State: classify(spent, limit, warn)})
	return out
}

// Enforce returns an error wrapping ErrBudgetExceeded when the task's or the
// space's budget has been reached; otherwise nil. Use it before launching work.
func (t *Tracker) Enforce(space, task string) error {
	for _, s := range t.Check(space, task) {
		if s.State == BudgetExceeded {
			target := "space " + s.Space
			if s.Scope == ScopeTask {
				target = fmt.Sprintf("task %s/%s", s.Space, s.Task)
			}
			return fmt.Errorf("%w: %s spent $%.4f of $%.4f", ErrBudgetExceeded, target, s.Spent, s.Limit)
		}
	}
	return nil
}

type ledgerFile struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

const ledgerVersion = 1

// Save writes the ledger to path atomically (temp file + rename), creating
// parent directories.
func (t *Tracker) Save(path string) error {
	data, err := json.MarshalIndent(ledgerFile{Version: ledgerVersion, Entries: t.Entries()}, "", "  ")
	if err != nil {
		return fmt.Errorf("pi: encode ledger: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pi: create ledger dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ledger.*")
	if err != nil {
		return fmt.Errorf("pi: write ledger: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("pi: write ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pi: write ledger: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("pi: install ledger: %w", err)
	}
	return nil
}

// Load replaces the tracker's entries with those stored at path. A missing
// file is not an error (fresh ledger).
func (t *Tracker) Load(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pi: read ledger: %w", err)
	}
	var lf ledgerFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return fmt.Errorf("pi: decode ledger %s: %w", path, err)
	}
	if lf.Version != ledgerVersion {
		return fmt.Errorf("pi: ledger %s has unsupported version %d", path, lf.Version)
	}
	entries := make(map[entryKey]Entry, len(lf.Entries))
	for _, e := range lf.Entries {
		entries[entryKey{e.Space, e.Task, e.Session}] = e
	}
	t.mu.Lock()
	t.entries = entries
	t.mu.Unlock()
	return nil
}

// LedgerPath returns $XDG_DATA_HOME/fleet/ledger.json.
func LedgerPath() (string, error) {
	d, err := agent.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ledger.json"), nil
}
