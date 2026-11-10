package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"fleet/pkg/schema"
)

// ViewMode selects how the task pane lays out tasks.
type ViewMode int

const (
	ViewLanes ViewMode = iota
	ViewBacklog
)

// TaskViewLabels are the tab labels, indexed by ViewMode.
var TaskViewLabels = []string{"Lanes", "Backlog"}

// laneOrder lists the visible lanes. Closed tasks are shown in the Done lane.
var laneOrder = []schema.Status{
	schema.StatusQueued, schema.StatusWorking, schema.StatusReview, schema.StatusDone,
}

const (
	cardHeight   = 4 // title, branch, worker/spend, acceptance criteria
	cardStride   = cardHeight + 1
	laneHeadRows = 2 // header + rule
	minLaneWidth = 14
)

func laneOf(s schema.Status) int {
	if s == schema.StatusClosed {
		return len(laneOrder) - 1
	}
	for i, l := range laneOrder {
		if l == s {
			return i
		}
	}
	return 0
}

// Progress is how many acceptance criteria a reviewer has confirmed.
type Progress struct{ Met, Total int }

// TasksModel shows tasks as Queued/Working/Review/Done lanes or a flat backlog.
type TasksModel struct {
	styles   Styles
	mode     ViewMode
	order    []schema.Task // grouped by lane, input order kept inside a lane
	lanes    [4][]int      // indexes into order, per lane
	cursor   int           // index into order
	laneOff  [4]int
	listOff  int
	progress map[string]Progress

	width, height int
}

// NewTasks returns an empty task pane.
func NewTasks(st Styles) *TasksModel {
	return &TasksModel{styles: st, progress: map[string]Progress{}}
}

// SetSize sets the content area (inside the pane frame).
func (m *TasksModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.ensureVisible()
}

// Mode returns the current view mode.
func (m *TasksModel) Mode() ViewMode { return m.mode }

// SetMode switches between lanes and backlog.
func (m *TasksModel) SetMode(v ViewMode) {
	m.mode = v
	m.ensureVisible()
}

// effectiveMode falls back to the backlog when lanes would be unreadably narrow.
func (m *TasksModel) effectiveMode() ViewMode {
	if m.mode == ViewLanes && m.width < len(laneOrder)*minLaneWidth+len(laneOrder)-1 {
		return ViewBacklog
	}
	return m.mode
}

// SetTasks replaces the task set, keeping the selection on the same task ID.
func (m *TasksModel) SetTasks(tasks []schema.Task) {
	id := m.SelectedID()
	m.order = m.order[:0:0]
	m.lanes = [4][]int{}
	for l := range laneOrder {
		for _, t := range tasks {
			if laneOf(t.Status) == l {
				m.lanes[l] = append(m.lanes[l], len(m.order))
				m.order = append(m.order, t)
			}
		}
	}
	m.cursor = 0
	m.SelectByID(id)
	m.ensureVisible()
}

// SetProgress records reviewer progress for the acceptance-criteria indicator.
func (m *TasksModel) SetProgress(taskID string, p Progress) { m.progress[taskID] = p }

// Tasks returns the tasks in display order.
func (m *TasksModel) Tasks() []schema.Task { return append([]schema.Task(nil), m.order...) }

// Selected returns the highlighted task.
func (m *TasksModel) Selected() (schema.Task, bool) {
	if m.cursor < 0 || m.cursor >= len(m.order) {
		return schema.Task{}, false
	}
	return m.order[m.cursor], true
}

// SelectedID returns the highlighted task ID or "".
func (m *TasksModel) SelectedID() string {
	t, _ := m.Selected()
	return t.ID
}

// SelectByID highlights a task; it reports whether the task exists.
func (m *TasksModel) SelectByID(id string) bool {
	for i, t := range m.order {
		if id != "" && t.ID == id {
			m.cursor = i
			m.ensureVisible()
			return true
		}
	}
	return false
}

// Counts returns the number of tasks per lane.
func (m *TasksModel) Counts() [4]int {
	var c [4]int
	for i := range m.lanes {
		c[i] = len(m.lanes[i])
	}
	return c
}

// position returns the lane and row of the cursor.
func (m *TasksModel) position() (lane, row int) {
	for l, idx := range m.lanes {
		for r, i := range idx {
			if i == m.cursor {
				return l, r
			}
		}
	}
	return 0, 0
}

func (m *TasksModel) moveVertical(d int) {
	if len(m.order) == 0 {
		return
	}
	if m.effectiveMode() == ViewBacklog {
		m.cursor = clamp(m.cursor+d, 0, len(m.order)-1)
		return
	}
	lane, row := m.position()
	m.cursor = m.lanes[lane][clamp(row+d, 0, len(m.lanes[lane])-1)]
}

func (m *TasksModel) moveLane(d int) {
	if len(m.order) == 0 || m.effectiveMode() != ViewLanes {
		return
	}
	lane, row := m.position()
	for l := lane + d; l >= 0 && l < len(m.lanes); l += d {
		if n := len(m.lanes[l]); n > 0 {
			m.cursor = m.lanes[l][min(row, n-1)]
			return
		}
	}
}

// Update handles navigation keys.
func (m *TasksModel) Update(msg tea.Msg) tea.Cmd {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch km.String() {
	case "up", "k":
		m.moveVertical(-1)
	case "down", "j":
		m.moveVertical(1)
	case "left", "h":
		m.moveLane(-1)
	case "right":
		m.moveLane(1)
	case "home", "g":
		m.cursor = 0
		if m.effectiveMode() == ViewLanes {
			if lane, _ := m.position(); len(m.lanes[lane]) > 0 {
				m.cursor = m.lanes[lane][0]
			}
		}
	case "end", "G":
		m.cursor = max(0, len(m.order)-1)
		if m.effectiveMode() == ViewLanes {
			if lane, _ := m.position(); len(m.lanes[lane]) > 0 {
				m.cursor = m.lanes[lane][len(m.lanes[lane])-1]
			}
		}
	default:
		return nil
	}
	m.ensureVisible()
	return nil
}

func (m *TasksModel) laneVisible() int { return max(1, (m.height-laneHeadRows+1)/cardStride) }
func (m *TasksModel) listVisible() int { return max(1, (m.height-1)/2) }

func (m *TasksModel) ensureVisible() {
	if len(m.order) == 0 {
		m.cursor, m.listOff, m.laneOff = 0, 0, [4]int{}
		return
	}
	m.cursor = clamp(m.cursor, 0, len(m.order)-1)
	lane, row := m.position()
	v := m.laneVisible()
	for l := range m.laneOff {
		m.laneOff[l] = clamp(m.laneOff[l], 0, max(0, len(m.lanes[l])-v))
	}
	if row < m.laneOff[lane] {
		m.laneOff[lane] = row
	}
	if row >= m.laneOff[lane]+v {
		m.laneOff[lane] = row - v + 1
	}
	lv := m.listVisible()
	if m.cursor < m.listOff {
		m.listOff = m.cursor
	}
	if m.cursor >= m.listOff+lv {
		m.listOff = m.cursor - lv + 1
	}
	m.listOff = clamp(m.listOff, 0, max(0, len(m.order)-lv))
}

// acIndicator renders the acceptance-criteria badge of a task.
func (m *TasksModel) acIndicator(t schema.Task) string {
	n := len(t.AcceptanceCriteria)
	if n == 0 {
		return m.styles.Danger.Render("⚠ no AC")
	}
	if p, ok := m.progress[t.ID]; ok && p.Total > 0 {
		st := m.styles.Warning
		icon := "◐"
		if p.Met >= p.Total {
			st, icon = m.styles.Success, "☑"
		} else if p.Met == 0 {
			st, icon = m.styles.Danger, "☐"
		}
		return st.Render(fmt.Sprintf("%s AC %d/%d", icon, p.Met, p.Total))
	}
	return m.styles.Subtle.Render(fmt.Sprintf("☐ AC %d", n))
}

func branchOrDash(t schema.Task) string {
	if t.Branch == "" {
		return "no branch"
	}
	return t.Branch
}

func workerOrDash(t schema.Task) string {
	if t.AssignedWorker == "" {
		return "no worker"
	}
	return "@" + t.AssignedWorker
}

// View renders the lanes or backlog.
func (m *TasksModel) View() string {
	if len(m.order) == 0 {
		return m.styles.Subtle.Render(wrap("No tasks yet. Press [n] to create one.", max(1, m.width)))
	}
	if m.effectiveMode() == ViewBacklog {
		return m.viewBacklog()
	}
	return m.viewLanes()
}

func (m *TasksModel) viewLanes() string {
	n := len(laneOrder)
	lw := (m.width - (n - 1)) / n
	cols := make([]string, 0, 2*n-1)
	for l, status := range laneOrder {
		if l > 0 {
			cols = append(cols, " ")
		}
		col := lipgloss.NewStyle().Width(lw).MaxWidth(lw).Render(m.renderLane(l, status, lw))
		cols = append(cols, col)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cols...)
}

func (m *TasksModel) renderLane(l int, status schema.Status, w int) string {
	color := StatusColor(status)
	head := lipgloss.NewStyle().Bold(true).Foreground(color).Render(strings.ToUpper(string(status))) +
		m.styles.Subtle.Render(fmt.Sprintf(" %d", len(m.lanes[l])))
	rule := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("─", w))
	lines := []string{truncate(head, w), rule}
	idx := m.lanes[l]
	end := min(len(idx), m.laneOff[l]+m.laneVisible())
	for r := m.laneOff[l]; r < end; r++ {
		lines = append(lines, m.renderCard(m.order[idx[r]], idx[r] == m.cursor, w), "")
	}
	return strings.Join(lines, "\n")
}

func (m *TasksModel) renderCard(t schema.Task, selected bool, w int) string {
	bar := m.styles.Subtle.Render("│ ")
	title := m.styles.Text
	if selected {
		bar = lipgloss.NewStyle().Foreground(StatusColor(t.Status)).Render("┃ ")
		title = m.styles.Bold
	}
	iw := max(1, w-2)
	sub := m.styles.Subtle
	rows := []string{
		title.Render(truncate(t.Title, iw)),
		sub.Render(truncate("⎇ "+branchOrDash(t), iw)),
		spread(sub.Render(workerOrDash(t)), sub.Render(FormatUSD(t.SpendUSD)), iw),
		m.acIndicator(t),
	}
	for i, r := range rows {
		rows[i] = bar + truncate(r, iw)
	}
	if t.Status == schema.StatusClosed {
		rows[0] = bar + sub.Render(truncate("✕ "+t.Title, iw))
	}
	return strings.Join(rows, "\n")
}

func (m *TasksModel) viewBacklog() string {
	lines := make([]string, 0, m.height)
	var counts []string
	for l, s := range laneOrder {
		counts = append(counts, lipgloss.NewStyle().Foreground(StatusColor(s)).Render(fmt.Sprintf("%s %d", s, len(m.lanes[l]))))
	}
	lines = append(lines, truncate(strings.Join(counts, m.styles.Subtle.Render(" · ")), m.width))
	end := min(len(m.order), m.listOff+m.listVisible())
	for i := m.listOff; i < end; i++ {
		t := m.order[i]
		marker := "  "
		title := m.styles.Text
		if i == m.cursor {
			marker, title = m.styles.Accent.Render("▸ "), m.styles.Bold
		}
		iw := max(1, m.width-2)
		l1 := spread(m.styles.StatusTag(t.Status)+" "+title.Render(t.Title), m.styles.Subtle.Render(FormatUSD(t.SpendUSD)), iw)
		l2 := "  " + m.styles.Subtle.Render(workerOrDash(t)+" · ⎇ "+branchOrDash(t)) + "  " + m.acIndicator(t)
		lines = append(lines, marker+truncate(l1, iw), "  "+truncate(l2, iw))
	}
	return strings.Join(lines, "\n")
}
