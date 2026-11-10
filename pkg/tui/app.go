package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"fleet/pkg/schema"
)

// Focus identifies the pane that receives keyboard input.
type Focus int

// Panes in focus-cycle order: Spaces -> Tasks -> Inspector -> Chat.
const (
	FocusSpaces Focus = iota
	FocusTasks
	FocusInspector
	FocusChat
	focusCount
)

func (f Focus) String() string {
	switch f {
	case FocusSpaces:
		return "spaces"
	case FocusTasks:
		return "tasks"
	case FocusInspector:
		return "inspector"
	case FocusChat:
		return "chat"
	}
	return "unknown"
}

const (
	minWidth        = 40
	minHeight       = 12
	wideWidth       = 100 // below this only the focused pane is shown
	wideBodyHeight  = 14
	footerRows      = 2 // status line + help line
	headerRows      = 1
	loadTimeout     = 15 * time.Second
	actionTimeout   = 2 * time.Minute
	defaultRefresh  = 2 * time.Second
	statusErrorHint = "✗ "
)

// Options configures the root model.
type Options struct {
	Backend         Backend         // nil behaves like NopBackend
	Context         context.Context // cancels in-flight backend calls; default Background
	RefreshInterval time.Duration   // polling period; default 2s, negative disables
}

type keyMap struct {
	Focus, FocusBack, Delegate, New, Approve, Review, Worktree, Lead, View,
	Help, Quit, Up, Down, Left, Right, Send, Newline, Back key.Binding
}

func newKeyMap() keyMap {
	b := func(keys, k, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(strings.Split(keys, ",")...), key.WithHelp(k, desc))
	}
	return keyMap{
		Focus:     b("tab", "tab", "next pane"),
		FocusBack: b("shift+tab", "shift+tab", "prev pane"),
		Delegate:  b("d", "d", "delegate"),
		New:       b("n", "n", "new task"),
		Approve:   b("a", "a", "approve/merge"),
		Review:    b("r", "r", "review"),
		Worktree:  b("w", "w", "worktree"),
		Lead:      b("l", "l", "lead chat"),
		View:      b("v", "v", "lanes/backlog"),
		Help:      b("?", "?", "help"),
		Quit:      b("q", "q", "quit"),
		Up:        b("up,k", "↑/k", "up"),
		Down:      b("down,j", "↓/j", "down"),
		Left:      b("left,h", "←/→", "lane/tab"),
		Right:     b("right", "→", "lane/tab"),
		Send:      b("enter", "enter", "send"),
		Newline:   b("ctrl+j", "ctrl+j", "newline"),
		Back:      b("esc", "esc", "back"),
	}
}

type overlay int

const (
	overlayNone overlay = iota
	overlayTitle
	overlayBrief
	overlayConfirm
)

type tickMsg time.Time

// Model is the root Bubble Tea model.
type Model struct {
	backend  Backend
	ctx      context.Context
	refresh  time.Duration
	styles   Styles
	keys     keyMap
	help     help.Model
	spaces   *SpacesModel
	tasks    *TasksModel
	inspect  *InspectorModel
	chat     *ChatModel
	focus    Focus
	width    int
	height   int
	showHelp bool

	status    string
	statusErr bool

	overlay    overlay
	input      textinput.Model
	newTitle   string
	confirm    string
	confirmRun func() tea.Cmd

	refreshing  bool
	selectAfter string // task ID to highlight once the next task load arrives
}

// New builds the root model.
func New(opts Options) Model {
	st := NewStyles()
	be := opts.Backend
	if be == nil {
		be = NopBackend{}
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	refresh := opts.RefreshInterval
	if refresh == 0 {
		refresh = defaultRefresh
	}
	h := help.New()
	h.Styles.ShortKey = st.Accent
	h.Styles.ShortDesc = st.Subtle
	h.Styles.ShortSeparator = st.Subtle
	h.Styles.FullKey = st.Accent
	h.Styles.FullDesc = st.Subtle
	h.Styles.FullSeparator = st.Subtle

	in := textinput.New()
	in.Prompt = "› "
	in.PromptStyle = st.Prompt
	in.CharLimit = 500

	m := Model{
		backend: be, ctx: ctx, refresh: refresh, styles: st, keys: newKeyMap(), help: h,
		spaces:  NewSpaces(st),
		tasks:   NewTasks(st),
		inspect: NewInspector(st),
		chat:    NewChat(ctx, st, be),
		focus:   FocusSpaces,
		input:   in,
	}
	return m
}

// Run starts the TUI on the alternate screen and blocks until it exits.
func Run(opts Options) error {
	_, err := tea.NewProgram(New(opts), tea.WithAltScreen(), tea.WithContext(opts.Context)).Run()
	return err
}

// Focus returns the focused pane.
func (m Model) Focus() Focus { return m.focus }

// Init starts the first load and the refresh ticker.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.loadSpaces(), m.tick())
}

func (m Model) tick() tea.Cmd {
	if m.refresh <= 0 {
		return nil
	}
	return tea.Tick(m.refresh, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---- backend commands ----

func (m Model) loadSpaces() tea.Cmd {
	be, ctx := m.backend, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, loadTimeout)
		defer cancel()
		s, err := be.Spaces(ctx)
		return SpacesMsg{Spaces: s, Err: err}
	}
}

func (m Model) loadTasks(spaceID string) tea.Cmd {
	be, ctx := m.backend, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, loadTimeout)
		defer cancel()
		t, err := be.Tasks(ctx, spaceID)
		return TasksMsg{SpaceID: spaceID, Tasks: t, Err: err}
	}
}

func (m Model) loadDetail(t schema.Task) tea.Cmd {
	be, ctx := m.backend, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, loadTimeout)
		defer cancel()
		d, err := be.Detail(ctx, t)
		return DetailMsg{TaskID: t.ID, Detail: d, Err: err}
	}
}

// action runs fn in the background and reports the result as an ActionMsg.
func (m Model) action(label string, fn func(ctx context.Context) (string, error)) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, actionTimeout)
		defer cancel()
		id, err := fn(ctx)
		return ActionMsg{Label: label, TaskID: id, Err: err}
	}
}

// startRefresh begins a spaces -> tasks -> detail load chain.
func (m *Model) startRefresh() tea.Cmd {
	m.refreshing = true
	return m.loadSpaces()
}

// ---- sizing ----

type layout struct {
	compact                 bool
	spacesW, centerW, inspW int
	tasksH, chatH, bodyH    int
}

func (m Model) layout() layout {
	l := layout{bodyH: max(0, m.height-headerRows-footerRows)}
	if m.width < wideWidth || l.bodyH < wideBodyHeight {
		l.compact = true
		return l
	}
	l.spacesW = clamp(m.width/5, 26, 34)
	l.inspW = clamp(m.width*30/100, 34, 60)
	l.centerW = m.width - l.spacesW - l.inspW
	l.chatH = clamp(l.bodyH*2/5, 7, l.bodyH-7)
	l.tasksH = l.bodyH - l.chatH
	return l
}

// resize propagates the terminal size to the panes (content = frame - 4x3).
func (m Model) resize() {
	l := m.layout()
	in := func(w, h int) (int, int) { return max(1, w-4), max(1, h-3) }
	if l.compact {
		w, h := in(m.width, l.bodyH)
		m.spaces.SetSize(w, h)
		m.tasks.SetSize(w, h)
		m.inspect.SetSize(w, h)
		m.chat.SetSize(w, h)
		return
	}
	m.spaces.SetSize(in(l.spacesW, l.bodyH))
	m.tasks.SetSize(in(l.centerW, l.tasksH))
	m.inspect.SetSize(in(l.inspW, l.bodyH))
	m.chat.SetSize(in(l.centerW, l.chatH))
}

// ---- update ----

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		m.resize()
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case tickMsg:
		cmds := []tea.Cmd{m.tick()}
		if !m.refreshing {
			cmds = append(cmds, m.startRefresh())
		}
		return m, tea.Batch(cmds...)
	case SpacesMsg:
		return m.onSpaces(msg)
	case TasksMsg:
		return m.onTasks(msg)
	case DetailMsg:
		m.refreshing = false
		if msg.Err != nil {
			m.setError("detail: " + msg.Err.Error())
		}
		m.inspect.SetDetail(msg.TaskID, msg.Detail, msg.Err)
		if msg.Err == nil && msg.Detail.Verdict != nil {
			m.setProgress(msg.TaskID, msg.Detail.Verdict)
		}
		return m, nil
	case ActionMsg:
		if msg.Err != nil {
			m.setError(msg.Label + ": " + msg.Err.Error())
			return m, nil
		}
		m.setStatus(msg.Label)
		m.selectAfter = msg.TaskID
		next := m.startRefresh()
		return m, next
	case EventMsg:
		return m.onEvent(msg.Event)
	case ChatReplyMsg:
		cmd := m.chat.Update(msg)
		// The lead may have created or changed tasks.
		return m, tea.Batch(cmd, m.loadTasks(m.spaces.SelectedID()))
	case tea.MouseMsg:
		return m, nil
	}
	// Spinner ticks, cursor blinks and anything else belongs to the chat (and
	// the prompt while it is open).
	cmds := []tea.Cmd{m.chat.Update(msg)}
	if m.overlay == overlayTitle || m.overlay == overlayBrief {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m Model) onSpaces(msg SpacesMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		m.refreshing = false
		m.setError("spaces: " + msg.Err.Error())
		return m, nil
	}
	m.spaces.SetSpaces(msg.Spaces)
	next := m.syncSpace()
	return m, next
}

// syncSpace points chat and tasks at the selected space and loads its tasks.
func (m *Model) syncSpace() tea.Cmd {
	s, ok := m.spaces.Selected()
	if !ok {
		m.refreshing = false
		m.chat.SetSpace("", "")
		m.tasks.SetTasks(nil)
		m.inspect.SetTask(schema.Task{}, false)
		return nil
	}
	m.chat.SetSpace(s.ID, s.Name)
	return m.loadTasks(s.ID)
}

func (m Model) onTasks(msg TasksMsg) (tea.Model, tea.Cmd) {
	if msg.SpaceID != m.spaces.SelectedID() {
		return m, nil // stale: the user moved to another space
	}
	if msg.Err != nil {
		m.refreshing = false
		m.setError("tasks: " + msg.Err.Error())
		return m, nil
	}
	m.tasks.SetTasks(msg.Tasks)
	if m.selectAfter != "" && m.tasks.SelectByID(m.selectAfter) {
		m.selectAfter = ""
	}
	next := m.syncTask()
	return m, next
}

// syncTask points the inspector at the selected task and loads its detail.
func (m *Model) syncTask() tea.Cmd {
	t, ok := m.tasks.Selected()
	m.inspect.SetTask(t, ok)
	if !ok {
		m.refreshing = false
		return nil
	}
	if v := m.inspect.Verdict(); v != nil {
		m.setProgress(t.ID, v)
	}
	return m.loadDetail(t)
}

func (m Model) setProgress(taskID string, v *schema.ReviewVerdict) {
	p := Progress{Total: len(v.Checklist)}
	for _, it := range v.Checklist {
		if it.Met {
			p.Met++
		}
	}
	m.tasks.SetProgress(taskID, p)
}

func (m Model) onEvent(ev schema.AgentEvent) (tea.Model, tea.Cmd) {
	line := fmt.Sprintf("%s %s %s", ev.Timestamp.Format("15:04:05"), ev.Role, ev.Type)
	if ev.Message != "" {
		line += ": " + ev.Message
	}
	m.inspect.AppendLog(ev.TaskID, line)
	if ev.Verdict != nil {
		m.inspect.SetVerdict(ev.TaskID, ev.Verdict)
		m.setProgress(ev.TaskID, ev.Verdict)
	}
	if ev.SpaceID != m.spaces.SelectedID() || m.refreshing {
		return m, nil
	}
	next := m.startRefresh()
	return m, next
}

func (m *Model) setStatus(s string) { m.status, m.statusErr = s, false }
func (m *Model) setError(s string)  { m.status, m.statusErr = s, true }

// setFocus moves focus and returns the command the new pane needs.
func (m *Model) setFocus(f Focus) tea.Cmd {
	m.focus = (f%focusCount + focusCount) % focusCount
	if m.focus == FocusChat {
		return m.chat.Focus()
	}
	m.chat.Blur()
	return nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.overlay != overlayNone {
		return m.updateOverlay(msg)
	}
	m.status, m.statusErr = "", false
	switch {
	case key.Matches(msg, m.keys.Focus):
		next := m.setFocus(m.focus + 1)
		return m, next
	case key.Matches(msg, m.keys.FocusBack):
		next := m.setFocus(m.focus - 1)
		return m, next
	}
	if m.focus == FocusChat {
		if key.Matches(msg, m.keys.Back) {
			next := m.setFocus(FocusTasks)
			return m, next
		}
		return m, m.chat.Update(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.showHelp = !m.showHelp
		return m, nil
	case key.Matches(msg, m.keys.Back):
		m.showHelp = false
		return m, nil
	case key.Matches(msg, m.keys.Lead):
		next := m.setFocus(FocusChat)
		return m, next
	case key.Matches(msg, m.keys.View):
		m.tasks.SetMode(ViewMode((int(m.tasks.Mode()) + 1) % len(TaskViewLabels)))
		return m, nil
	case key.Matches(msg, m.keys.New):
		next := m.openPrompt()
		return m, next
	case key.Matches(msg, m.keys.Delegate):
		return m.doDelegate()
	case key.Matches(msg, m.keys.Review):
		return m.doReview()
	case key.Matches(msg, m.keys.Approve):
		return m.doApprove()
	case key.Matches(msg, m.keys.Worktree):
		return m.doWorktree()
	}
	return m.forwardToFocused(msg)
}

// forwardToFocused routes navigation keys and syncs dependent panes when the
// selection moved.
func (m Model) forwardToFocused(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.focus {
	case FocusSpaces:
		before := m.spaces.SelectedID()
		cmd := m.spaces.Update(msg)
		if m.spaces.SelectedID() != before {
			m.tasks.SetTasks(nil)
			m.inspect.SetTask(schema.Task{}, false)
			next := tea.Batch(cmd, m.syncSpace())
			return m, next
		}
		return m, cmd
	case FocusTasks:
		before := m.tasks.SelectedID()
		cmd := m.tasks.Update(msg)
		if m.tasks.SelectedID() != before {
			next := tea.Batch(cmd, m.syncTask())
			return m, next
		}
		return m, cmd
	case FocusInspector:
		return m, m.inspect.Update(msg)
	}
	return m, nil
}

// ---- actions ----

func (m Model) doDelegate() (tea.Model, tea.Cmd) {
	t, ok := m.tasks.Selected()
	switch {
	case !ok:
		m.setError("no task selected")
	case !t.Status.CanTransition(schema.StatusWorking) || t.Status != schema.StatusQueued:
		m.setError(fmt.Sprintf("cannot delegate a %s task (only queued)", t.Status))
	default:
		return m, m.action("delegated "+quote(t.Title), func(ctx context.Context) (string, error) {
			return t.ID, m.backend.Delegate(ctx, t)
		})
	}
	return m, nil
}

func (m Model) doReview() (tea.Model, tea.Cmd) {
	t, ok := m.tasks.Selected()
	switch {
	case !ok:
		m.setError("no task selected")
	case t.Status == schema.StatusReview:
		// Already in review: show the verdict.
		m.inspect.SetTab(TabReview)
		next := m.setFocus(FocusInspector)
		return m, next
	case !t.Status.CanTransition(schema.StatusReview):
		m.setError(fmt.Sprintf("cannot review a %s task (only working)", t.Status))
	default:
		return m, m.action("review requested for "+quote(t.Title), func(ctx context.Context) (string, error) {
			return t.ID, m.backend.RequestReview(ctx, t)
		})
	}
	return m, nil
}

func (m Model) doApprove() (tea.Model, tea.Cmd) {
	t, ok := m.tasks.Selected()
	switch {
	case !ok:
		m.setError("no task selected")
	case !t.Status.CanTransition(schema.StatusDone):
		m.setError(fmt.Sprintf("cannot approve a %s task (only review)", t.Status))
	default:
		q := "Approve and merge " + quote(t.Title) + "?"
		if v := m.inspect.Verdict(); v == nil {
			q = "No reviewer verdict yet. " + q
		} else if v.Verdict != schema.DecisionApproved {
			q = "Reviewer requested changes. " + q
		}
		m.overlay, m.confirm = overlayConfirm, q
		m.confirmRun = func() tea.Cmd {
			return m.action("approved "+quote(t.Title), func(ctx context.Context) (string, error) {
				return t.ID, m.backend.Approve(ctx, t)
			})
		}
	}
	return m, nil
}

func (m Model) doWorktree() (tea.Model, tea.Cmd) {
	t, ok := m.tasks.Selected()
	switch {
	case !ok:
		m.setError("no task selected")
	case t.WorktreePath == "":
		m.setError("task has no worktree yet")
	default:
		m.inspect.SetTab(TabDiff)
		m.setStatus("worktree: " + t.WorktreePath)
		next := m.setFocus(FocusInspector)
		return m, next
	}
	return m, nil
}

// ---- overlays (new-task prompt, confirmation) ----

func (m *Model) openPrompt() tea.Cmd {
	if m.spaces.SelectedID() == "" {
		m.setError("no space selected")
		return nil
	}
	m.overlay = overlayTitle
	m.newTitle = ""
	m.input.Reset()
	m.input.Placeholder = "task title"
	return m.input.Focus()
}

func (m Model) updateOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.overlay == overlayConfirm {
		switch strings.ToLower(msg.String()) {
		case "y":
			run := m.confirmRun
			m.overlay, m.confirm, m.confirmRun = overlayNone, "", nil
			if run != nil {
				return m, run()
			}
		case "n", "esc", "enter", "q":
			m.overlay, m.confirm, m.confirmRun = overlayNone, "", nil
			m.setStatus("cancelled")
		}
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		m.input.Blur()
		m.setStatus("cancelled")
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		if m.overlay == overlayTitle {
			if val == "" {
				m.setError("title is required")
				return m, nil
			}
			m.newTitle = val
			m.overlay = overlayBrief
			m.input.Reset()
			m.input.Placeholder = "brief (optional, enter to skip)"
			return m, nil
		}
		m.overlay = overlayNone
		m.input.Blur()
		spaceID, title, brief := m.spaces.SelectedID(), m.newTitle, val
		if brief == "" {
			brief = title
		}
		return m, m.action("created "+quote(title), func(ctx context.Context) (string, error) {
			t, err := m.backend.CreateTask(ctx, spaceID, title, brief)
			return t.ID, err
		})
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func quote(s string) string { return "“" + truncate(s, 40) + "”" }

// ---- view ----

// View implements tea.Model.
func (m Model) View() string {
	if m.width < minWidth || m.height < minHeight {
		return fmt.Sprintf("Terminal too small (%d×%d); need at least %d×%d.", m.width, m.height, minWidth, minHeight)
	}
	l := m.layout()
	rows := []string{m.header(), m.body(l), m.statusLine(), m.footer()}
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(strings.Join(rows, "\n"))
}

func (m Model) header() string {
	spent, budget := m.spaces.Totals()
	left := m.styles.Title.Render("◆ fleet") + "  " + m.styles.Tabs(TaskViewLabels, int(m.tasks.Mode()))
	if m.layout().compact {
		left += "  " + m.styles.Subtle.Render("pane: ") + m.styles.Accent.Render(m.focus.String())
	}
	right := m.styles.Subtle.Render(fmt.Sprintf("%d spaces · Σ ", len(m.spaces.Spaces()))) +
		lipgloss.NewStyle().Foreground(budgetColor(spent, budget)).Render(SpendLabel(spent, budget))
	return spread(left, right, m.width)
}

func (m Model) body(l layout) string {
	if l.compact {
		return m.pane(m.focus, m.width, l.bodyH)
	}
	center := lipgloss.JoinVertical(lipgloss.Left,
		m.pane(FocusTasks, l.centerW, l.tasksH),
		m.pane(FocusChat, l.centerW, l.chatH),
	)
	return lipgloss.JoinHorizontal(lipgloss.Top,
		m.pane(FocusSpaces, l.spacesW, l.bodyH),
		center,
		m.pane(FocusInspector, l.inspW, l.bodyH),
	)
}

// pane renders one framed pane at the given outer size.
func (m Model) pane(f Focus, w, h int) string {
	focused := m.focus == f
	switch f {
	case FocusSpaces:
		return m.styles.Pane("Spaces", m.styles.Subtle.Render(fmt.Sprint(len(m.spaces.Spaces()))), m.spaces.View(), w, h, focused)
	case FocusTasks:
		c := m.tasks.Counts()
		aside := m.styles.Subtle.Render(fmt.Sprintf("%d tasks", c[0]+c[1]+c[2]+c[3]))
		return m.styles.Pane("Tasks", aside, m.tasks.View(), w, h, focused)
	case FocusInspector:
		aside := ""
		if id := m.inspect.TaskID(); id != "" {
			aside = m.styles.Subtle.Render(truncate(id, max(1, w/2)))
		}
		return m.styles.Pane("Inspector", aside, m.inspect.View(), w, h, focused)
	default:
		title, aside := "Lead chat", ""
		if s, ok := m.spaces.Selected(); ok {
			title = "Lead · " + s.Name
			aside = m.styles.LeadDot(s.Lead) + " " + m.styles.Subtle.Render(s.Lead.String())
		}
		return m.styles.Pane(title, aside, m.chat.View(), w, h, focused)
	}
}

func (m Model) statusLine() string {
	switch {
	case m.overlay == overlayConfirm:
		return truncate(m.styles.Warning.Render(m.confirm)+m.styles.Subtle.Render("  [y] yes · [n] no"), m.width)
	case m.overlay == overlayTitle || m.overlay == overlayBrief:
		label := "New task title"
		if m.overlay == overlayBrief {
			label = "Brief for " + quote(m.newTitle)
		}
		m.input.Width = max(10, m.width-lipgloss.Width(label)-6)
		return truncate(m.styles.Accent.Render(label)+" "+m.input.View(), m.width)
	case m.showHelp:
		k := m.keys
		return m.help.ShortHelpView([]key.Binding{k.Up, k.Down, k.Left, k.Focus, k.FocusBack, k.View})
	case m.status == "":
		return ""
	case m.statusErr:
		return truncate(m.styles.StatusError.Render(statusErrorHint+m.status), m.width)
	}
	return truncate(m.styles.Status.Render(m.status), m.width)
}

func (m Model) footer() string {
	k := m.keys
	switch {
	case m.overlay == overlayConfirm:
		return m.help.ShortHelpView([]key.Binding{key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")), key.NewBinding(key.WithKeys("n"), key.WithHelp("n/esc", "cancel"))})
	case m.overlay != overlayNone:
		return m.help.ShortHelpView([]key.Binding{k.Send, k.Back})
	case m.focus == FocusChat:
		return m.help.ShortHelpView([]key.Binding{k.Send, k.Newline, k.Focus, k.Back})
	case m.showHelp:
		return m.help.ShortHelpView([]key.Binding{
			k.Delegate, k.New, k.Approve, k.Review, k.Worktree, k.Lead, k.Help, k.Quit,
		})
	}
	return m.help.ShortHelpView([]key.Binding{
		k.Focus, k.Delegate, k.New, k.Approve, k.Review, k.Worktree, k.Lead, k.Help, k.Quit,
	})
}
