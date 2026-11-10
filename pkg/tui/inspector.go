package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"fleet/pkg/schema"
)

// InspectorTab selects which facet of a task the inspector shows.
type InspectorTab int

const (
	TabOverview InspectorTab = iota // brief, criteria, worktree
	TabDiff                         // live git diff
	TabLogs                         // recent worker turns
	TabReview                       // reviewer verdict checklist
)

// InspectorTabLabels are the tab labels, indexed by InspectorTab.
var InspectorTabLabels = []string{"Brief", "Diff", "Logs", "Review"}

const (
	maxDiffLines = 5000
	maxLogLines  = 500
)

// InspectorModel is the right-hand detail pane for the selected task.
type InspectorModel struct {
	styles  Styles
	task    schema.Task
	hasTask bool
	detail  TaskDetail
	live    []string // events received since the last detail load
	loaded  bool
	err     string
	tab     InspectorTab
	vp      viewport.Model
	width   int
	height  int
}

// NewInspector returns an empty inspector.
func NewInspector(st Styles) *InspectorModel {
	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true
	return &InspectorModel{styles: st, vp: vp}
}

// SetSize sets the content area (inside the pane frame).
func (m *InspectorModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.vp.Width = max(1, w)
	m.vp.Height = max(1, h-1) // tab bar
	m.rebuild(true)
}

// Tab returns the active tab.
func (m *InspectorModel) Tab() InspectorTab { return m.tab }

// SetTab switches tabs and scrolls to the top.
func (m *InspectorModel) SetTab(t InspectorTab) {
	n := InspectorTab(len(InspectorTabLabels))
	m.tab = (t%n + n) % n
	m.rebuild(false)
	m.vp.GotoTop()
}

// SetTask points the inspector at a task. Selecting a different task clears
// the cached detail; refreshing the same task keeps scroll position.
func (m *InspectorModel) SetTask(t schema.Task, ok bool) {
	changed := ok != m.hasTask || t.ID != m.task.ID
	m.task, m.hasTask = t, ok
	if changed {
		m.detail, m.live, m.loaded, m.err = TaskDetail{}, nil, false, ""
	}
	m.rebuild(false)
	if changed {
		m.vp.GotoTop()
	}
}

// TaskID returns the inspected task ID or "".
func (m *InspectorModel) TaskID() string {
	if !m.hasTask {
		return ""
	}
	return m.task.ID
}

// SetDetail installs freshly loaded detail for the inspected task. Events
// that arrived before the load are superseded.
func (m *InspectorModel) SetDetail(taskID string, d TaskDetail, err error) {
	if !m.hasTask || taskID != m.task.ID {
		return
	}
	if err != nil {
		m.err = err.Error()
	} else {
		m.detail, m.live, m.loaded, m.err = d, nil, true, ""
	}
	m.rebuild(false)
}

// AppendLog adds a live log line for the inspected task.
func (m *InspectorModel) AppendLog(taskID, line string) {
	if !m.hasTask || taskID != m.task.ID {
		return
	}
	m.live = append(m.live, line)
	if len(m.live) > maxLogLines {
		m.live = m.live[len(m.live)-maxLogLines:]
	}
	m.rebuild(false)
}

// SetVerdict overrides the verdict (used for live review_verdict events).
func (m *InspectorModel) SetVerdict(taskID string, v *schema.ReviewVerdict) {
	if !m.hasTask || taskID != m.task.ID {
		return
	}
	m.detail.Verdict = v
	m.rebuild(false)
}

// Verdict returns the known verdict of the inspected task.
func (m *InspectorModel) Verdict() *schema.ReviewVerdict { return m.detail.Verdict }

// Update handles tab switching and scrolling.
func (m *InspectorModel) Update(msg tea.Msg) tea.Cmd {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "left", "[", "shift+tab":
			m.SetTab(m.tab - 1)
			return nil
		case "right", "]":
			m.SetTab(m.tab + 1)
			return nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

// View renders the tab bar and the scrollable body.
func (m *InspectorModel) View() string {
	bar := m.styles.Tabs(InspectorTabLabels, int(m.tab))
	return truncate(bar, m.width) + "\n" + m.vp.View()
}

// rebuild regenerates the viewport content, preserving the scroll offset.
func (m *InspectorModel) rebuild(top bool) {
	if m.width <= 0 {
		return
	}
	off := m.vp.YOffset
	m.vp.SetContent(m.content())
	if top {
		off = 0
	}
	m.vp.SetYOffset(off)
}

func (m *InspectorModel) content() string {
	if !m.hasTask {
		return m.styles.Subtle.Render("Select a task to inspect it.")
	}
	var body string
	switch m.tab {
	case TabOverview:
		body = m.overview()
	case TabDiff:
		body = m.diff()
	case TabLogs:
		body = m.logs()
	case TabReview:
		body = m.review()
	}
	if m.err != "" {
		body = m.styles.StatusError.Render(wrap("load failed: "+sanitize(m.err), m.width)) + "\n\n" + body
	}
	return body
}

func (m *InspectorModel) kv(k, v string) string {
	if v == "" {
		v = "—"
	}
	return m.styles.Subtle.Render(fmt.Sprintf("%-9s", k)) + " " + m.styles.Text.Render(truncate(sanitize(v), max(1, m.width-10)))
}

func (m *InspectorModel) overview() string {
	t := m.task
	var b strings.Builder
	b.WriteString(m.styles.Bold.Render(wrap(sanitize(t.Title), m.width)) + "\n")
	b.WriteString(m.styles.StatusTag(t.Status) + " " + m.styles.Subtle.Render(t.ID) + "\n\n")
	b.WriteString(m.styles.Accent.Render("Brief") + "\n")
	if strings.TrimSpace(t.Brief) == "" {
		b.WriteString(m.styles.Subtle.Render("(no brief)") + "\n")
	} else {
		b.WriteString(wrap(sanitize(t.Brief), m.width) + "\n")
	}
	b.WriteString("\n" + m.styles.Accent.Render("Acceptance criteria") + "\n")
	if len(t.AcceptanceCriteria) == 0 {
		b.WriteString(m.styles.Danger.Render("⚠ none defined") + "\n")
	}
	for _, c := range t.AcceptanceCriteria {
		icon, st := "☐", m.styles.Subtle
		if met, known := m.criterionMet(c); known {
			if met {
				icon, st = "☑", m.styles.Success
			} else {
				icon, st = "✗", m.styles.Danger
			}
		}
		b.WriteString(st.Render(icon) + " " + hangingWrap(sanitize(c), m.width-2) + "\n")
	}
	b.WriteString("\n" + m.styles.Accent.Render("Worktree") + "\n")
	b.WriteString(m.kv("path", t.WorktreePath) + "\n")
	b.WriteString(m.kv("branch", t.Branch) + "\n")
	b.WriteString(m.kv("worker", t.AssignedWorker) + "\n")
	b.WriteString(m.kv("spend", FormatUSD(t.SpendUSD)) + "\n")
	if t.PRUrl != "" {
		b.WriteString(m.kv("pr", t.PRUrl) + "\n")
	}
	if t.JiraKey != "" {
		b.WriteString(m.kv("jira", t.JiraKey) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// hangingWrap wraps s to w cells and indents continuation lines by two.
func hangingWrap(s string, w int) string {
	return strings.ReplaceAll(wrap(s, max(1, w)), "\n", "\n  ")
}

// criterionMet looks a criterion up in the reviewer checklist.
func (m *InspectorModel) criterionMet(c string) (met, known bool) {
	if m.detail.Verdict == nil {
		return false, false
	}
	for _, it := range m.detail.Verdict.Checklist {
		if strings.EqualFold(strings.TrimSpace(it.Item), strings.TrimSpace(c)) {
			return it.Met, true
		}
	}
	return false, false
}

func (m *InspectorModel) diff() string {
	if !m.loaded {
		return m.styles.Subtle.Render("Loading diff…")
	}
	d := strings.TrimRight(sanitize(m.detail.Diff), "\n")
	if strings.TrimSpace(d) == "" {
		return m.styles.Subtle.Render("No changes in the worktree.")
	}
	lines := strings.Split(d, "\n")
	extra := 0
	if len(lines) > maxDiffLines {
		extra = len(lines) - maxDiffLines
		lines = lines[:maxDiffLines]
	}
	for i, l := range lines {
		lines[i] = m.styleDiffLine(truncate(l, m.width))
	}
	if extra > 0 {
		lines = append(lines, m.styles.Subtle.Render(fmt.Sprintf("… %d more lines", extra)))
	}
	return strings.Join(lines, "\n")
}

func (m *InspectorModel) styleDiffLine(l string) string {
	switch {
	case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"),
		strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "):
		return m.styles.DiffMeta.Render(l)
	case strings.HasPrefix(l, "@@"):
		return m.styles.DiffHunk.Render(l)
	case strings.HasPrefix(l, "+"):
		return m.styles.DiffAdd.Render(l)
	case strings.HasPrefix(l, "-"):
		return m.styles.DiffDel.Render(l)
	}
	return l
}

func (m *InspectorModel) logs() string {
	lines := append(append([]string(nil), m.detail.Logs...), m.live...)
	if len(lines) == 0 {
		if !m.loaded {
			return m.styles.Subtle.Render("Loading logs…")
		}
		return m.styles.Subtle.Render("No worker turns recorded yet.")
	}
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(wrap(sanitize(l), m.width))
	}
	return b.String()
}

func (m *InspectorModel) review() string {
	v := m.detail.Verdict
	if v == nil {
		return m.styles.Subtle.Render("No reviewer verdict yet.\nPress [r] to request a review.")
	}
	var b strings.Builder
	if v.Verdict == schema.DecisionApproved {
		b.WriteString(m.styles.Success.Bold(true).Render("✔ APPROVED"))
	} else {
		b.WriteString(m.styles.Danger.Bold(true).Render("✘ CHANGES REQUESTED"))
	}
	b.WriteString("\n\n")
	met := 0
	for _, it := range v.Checklist {
		if it.Met {
			met++
		}
	}
	b.WriteString(m.styles.Subtle.Render(fmt.Sprintf("%d/%d criteria met", met, len(v.Checklist))) + "\n")
	for _, it := range v.Checklist {
		icon, st := "☑", m.styles.Success
		if !it.Met {
			icon, st = "✗", m.styles.Danger
		}
		b.WriteString(st.Render(icon) + " " + hangingWrap(sanitize(it.Item), m.width-2) + "\n")
		if it.Note != "" {
			b.WriteString("  " + m.styles.Subtle.Render(strings.ReplaceAll(wrap(sanitize(it.Note), m.width-2), "\n", "\n  ")) + "\n")
		}
	}
	if v.Notes != "" {
		b.WriteString("\n" + m.styles.Accent.Render("Notes") + "\n" + wrap(sanitize(v.Notes), m.width))
	}
	return strings.TrimRight(b.String(), "\n")
}
