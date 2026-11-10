package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"fleet/pkg/schema"
)

// fakeBackend is a deterministic in-memory Backend.
type fakeBackend struct {
	mu        sync.Mutex
	spaces    []SpaceInfo
	tasks     map[string][]schema.Task
	details   map[string]TaskDetail
	chatFn    func(spaceID, text string) (string, error)
	history   map[string][]ChatMessage
	histErr   error
	histCalls []string
	calls     []string
	failAct   error
}

func (f *fakeBackend) rec(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeBackend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBackend) Spaces(context.Context) ([]SpaceInfo, error) { return f.spaces, nil }
func (f *fakeBackend) Tasks(_ context.Context, id string) ([]schema.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]schema.Task(nil), f.tasks[id]...), nil
}
func (f *fakeBackend) Detail(_ context.Context, t schema.Task) (TaskDetail, error) {
	return f.details[t.ID], nil
}
func (f *fakeBackend) Chat(_ context.Context, id, text string) (string, error) {
	f.rec("chat:" + id + ":" + text)
	if f.chatFn != nil {
		return f.chatFn(id, text)
	}
	return "echo " + text, nil
}
func (f *fakeBackend) ChatHistory(_ context.Context, id string) ([]ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.histCalls = append(f.histCalls, id) // kept out of calls: it fires on every load
	if f.histErr != nil {
		return nil, f.histErr
	}
	return append([]ChatMessage(nil), f.history[id]...), nil
}
func (f *fakeBackend) CreateTask(_ context.Context, space, title, brief string) (schema.Task, error) {
	f.rec(fmt.Sprintf("create:%s:%s:%s", space, title, brief))
	if f.failAct != nil {
		return schema.Task{}, f.failAct
	}
	t := schema.Task{ID: "new-1", SpaceID: space, Title: title, Brief: brief, Status: schema.StatusQueued}
	f.mu.Lock()
	f.tasks[space] = append(f.tasks[space], t)
	f.mu.Unlock()
	return t, nil
}
func (f *fakeBackend) Delegate(_ context.Context, t schema.Task) error {
	f.rec("delegate:" + t.ID)
	return f.failAct
}
func (f *fakeBackend) RequestReview(_ context.Context, t schema.Task) error {
	f.rec("review:" + t.ID)
	return f.failAct
}
func (f *fakeBackend) Approve(_ context.Context, t schema.Task) error {
	f.rec("approve:" + t.ID)
	return f.failAct
}

func sampleTasks() []schema.Task {
	return []schema.Task{
		{ID: "t1", SpaceID: "s1", Title: "Add login", Brief: "Implement login", Status: schema.StatusQueued,
			AcceptanceCriteria: []string{"form renders", "errors shown"}},
		{ID: "t2", SpaceID: "s1", Title: "Fix cache bug", Status: schema.StatusWorking, AssignedWorker: "w1",
			Branch: "fleet/t2", WorktreePath: "/tmp/wt/t2", AcceptanceCriteria: []string{"no stale reads"}, SpendUSD: 0.42},
		{ID: "t3", SpaceID: "s1", Title: "Refactor API", Status: schema.StatusReview, AssignedWorker: "w2",
			Branch: "fleet/t3", WorktreePath: "/tmp/wt/t3", AcceptanceCriteria: []string{"tests pass", "docs updated"}},
		{ID: "t4", SpaceID: "s1", Title: "Bump deps", Status: schema.StatusDone, AssignedWorker: "w1", Branch: "fleet/t4"},
		{ID: "t5", SpaceID: "s1", Title: "Second queued", Status: schema.StatusQueued},
	}
}

func newFake() *fakeBackend {
	return &fakeBackend{
		spaces: []SpaceInfo{
			{ID: "s1", Name: "alpha", SpendUSD: 12.5, BudgetUSD: 50, Lead: LeadIdle, ActiveTasks: 3},
			{ID: "s2", Name: "beta", SpendUSD: 45, BudgetUSD: 50, Lead: LeadWorking, ActiveTasks: 1},
		},
		tasks: map[string][]schema.Task{"s1": sampleTasks(), "s2": {
			{ID: "u1", SpaceID: "s2", Title: "Beta task", Status: schema.StatusQueued},
		}},
		details: map[string]TaskDetail{
			"t3": {
				Diff: "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n",
				Logs: []string{"turn 1: read files", "turn 2: edit"},
				Verdict: &schema.ReviewVerdict{Verdict: schema.DecisionChangesRequested, Checklist: []schema.ChecklistItem{
					{Item: "tests pass", Met: true}, {Item: "docs updated", Met: false, Note: "README missing"},
				}, Notes: "please fix docs"},
			},
		},
	}
}

// runCmd executes cmd, giving up after d (for timers and blinking cursors).
func runCmd(cmd tea.Cmd, d time.Duration) (tea.Msg, bool) {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m, true
	case <-time.After(d):
		return nil, false
	}
}

// settle feeds cmd and every follow-up message through Update until idle.
func settle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 200 {
			t.Fatal("settle: command loop did not terminate")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg, ok := runCmd(c, 100*time.Millisecond)
		if !ok || msg == nil {
			continue
		}
		switch v := msg.(type) {
		case tea.BatchMsg:
			queue = append(queue, v...)
		case spinner.TickMsg, tickMsg, tea.QuitMsg:
		default:
			var next tea.Cmd
			var nm tea.Model
			nm, next = m.Update(msg)
			m = nm.(Model)
			queue = append(queue, next)
		}
	}
	return m
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+j":
		return tea.KeyMsg{Type: tea.KeyCtrlJ}
	case "alt+enter":
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press sends one key through the root model and settles the commands.
func press(t *testing.T, m Model, k string) Model {
	t.Helper()
	nm, cmd := m.Update(keyMsg(k))
	return settle(t, nm.(Model), cmd)
}

func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = press(t, m, string(r))
	}
	return m
}

func resized(t *testing.T, m Model, w, h int) Model {
	t.Helper()
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(Model)
}

// loadedApp returns a model with fake data loaded at 140x40.
func loadedApp(t *testing.T) (Model, *fakeBackend) {
	t.Helper()
	fb := newFake()
	m := New(Options{Backend: fb, RefreshInterval: -1})
	m = resized(t, m, 140, 40)
	m = settle(t, m, m.Init())
	return m, fb
}

// assertBox checks that s occupies exactly w columns max and h lines.
func assertBox(t *testing.T, s string, w, h int) {
	t.Helper()
	if got := lipgloss.Height(s); got != h {
		t.Errorf("height = %d, want %d", got, h)
	}
	if got := lipgloss.Width(s); got > w {
		t.Errorf("width = %d, want <= %d", got, w)
	}
	for i, l := range splitLines(s) {
		if ansi.StringWidth(l) > w {
			t.Errorf("line %d width %d > %d: %q", i, ansi.StringWidth(l), w, l)
		}
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

var errBoom = errors.New("boom")

func lipglossWidth(s string) int { return lipgloss.Width(s) }

// assertWithin checks that s fits inside w×h cells.
func assertWithin(t *testing.T, s string, w, h int) {
	t.Helper()
	if got := lipgloss.Height(s); got > h {
		t.Errorf("height = %d, want <= %d", got, h)
	}
	for i, l := range splitLines(s) {
		if ansi.StringWidth(l) > w {
			t.Errorf("line %d width %d > %d: %q", i, ansi.StringWidth(l), w, l)
		}
	}
}
