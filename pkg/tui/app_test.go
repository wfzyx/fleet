package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"fleet/pkg/schema"
)

func TestAppInitialLoadChain(t *testing.T) {
	m, _ := loadedApp(t)
	if m.spaces.SelectedID() != "s1" {
		t.Errorf("space = %q", m.spaces.SelectedID())
	}
	if len(m.tasks.Tasks()) != 5 {
		t.Errorf("tasks = %d", len(m.tasks.Tasks()))
	}
	if m.inspect.TaskID() != "t1" {
		t.Errorf("inspector task = %q", m.inspect.TaskID())
	}
	if m.refreshing {
		t.Error("refresh chain should be finished")
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"fleet", "Spaces", "Tasks", "Inspector", "Lead · alpha", "Add login", "alpha", "beta"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
}

func TestAppViewFitsTerminalExactly(t *testing.T) {
	m, _ := loadedApp(t)
	for _, sz := range [][2]int{{140, 40}, {100, 30}, {200, 60}, {99, 30}, {80, 24}, {60, 20}, {40, 12}, {120, 20}} {
		for f := FocusSpaces; f < focusCount; f++ {
			m2 := resized(t, m, sz[0], sz[1])
			m2.setFocus(f)
			assertBox(t, m2.View(), sz[0], sz[1])
		}
	}
}

func TestAppTooSmall(t *testing.T) {
	m, _ := loadedApp(t)
	m = resized(t, m, 30, 8)
	if !strings.Contains(m.View(), "too small") {
		t.Error("expected too-small notice")
	}
	m = resized(t, m, 140, 40)
	if strings.Contains(m.View(), "too small") {
		t.Error("notice should clear after resize")
	}
}

func TestAppResponsiveLayout(t *testing.T) {
	m, _ := loadedApp(t)
	l := m.layout()
	if l.compact || l.leftW+l.centerW+l.inspW != 140 || l.spacesH+l.tasksH != l.bodyH || l.spacesH < 7 || l.spacesH > 10 {
		t.Errorf("wide layout wrong: %+v", l)
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "Spaces") || !strings.Contains(v, "Inspector") || !strings.Contains(v, "Lead") {
		t.Error("wide layout should show all panes")
	}
	m = resized(t, m, 80, 24)
	if !m.layout().compact {
		t.Fatal("80 columns should be compact")
	}
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "Spaces") || strings.Contains(v, "Inspector") {
		t.Errorf("compact layout should show only the focused pane:\n%s", v)
	}
	// Content sizes follow the resize.
	if m.tasks.width != 76 {
		t.Errorf("tasks width = %d, want 76", m.tasks.width)
	}
}

func TestAppFocusCycle(t *testing.T) {
	m, _ := loadedApp(t)
	want := []Focus{FocusTasks, FocusChat, FocusInspector, FocusSpaces}
	for i, f := range want {
		m = press(t, m, "tab")
		if m.Focus() != f {
			t.Fatalf("tab %d: focus = %s, want %s", i, m.Focus(), f)
		}
	}
	m = press(t, m, "shift+tab")
	if m.Focus() != FocusInspector {
		t.Errorf("shift+tab: focus = %s", m.Focus())
	}
	m = press(t, m, "shift+tab")
	if m.Focus() != FocusChat || !m.chat.Focused() {
		t.Errorf("shift+tab: focus = %s chatFocused=%v", m.Focus(), m.chat.Focused())
	}
	m = press(t, m, "tab")
	if m.Focus() != FocusInspector || m.chat.Focused() {
		t.Error("chat should blur when focus leaves")
	}
}

func TestAppSpaceSwitchLoadsTasksAndChat(t *testing.T) {
	m, _ := loadedApp(t)
	m = press(t, m, "down")
	if m.spaces.SelectedID() != "s2" {
		t.Fatalf("space = %s", m.spaces.SelectedID())
	}
	if ts := m.tasks.Tasks(); len(ts) != 1 || ts[0].ID != "u1" {
		t.Errorf("tasks = %+v", ts)
	}
	if m.inspect.TaskID() != "u1" {
		t.Errorf("inspector = %q", m.inspect.TaskID())
	}
	if !strings.Contains(ansi.Strip(m.View()), "Lead · beta") {
		t.Error("chat did not follow the space")
	}
}

func TestAppTaskSelectionDrivesInspector(t *testing.T) {
	m, _ := loadedApp(t)
	m = press(t, m, "tab") // tasks
	m = press(t, m, "down")
	m = press(t, m, "down")
	m = press(t, m, "down") // backlog order: t1 t5 t2 t3 t4
	if m.inspect.TaskID() != "t3" {
		t.Fatalf("inspector = %q", m.inspect.TaskID())
	}
	if m.inspect.Verdict() == nil {
		t.Error("verdict should have loaded")
	}
	if got := m.tasks.progress["t3"]; got != (Progress{Met: 1, Total: 2}) {
		t.Errorf("progress = %+v", got)
	}
}

func TestAppQuit(t *testing.T) {
	m, _ := loadedApp(t)
	for _, k := range []string{"q", "ctrl+c"} {
		_, cmd := m.Update(keyMsg(k))
		if cmd == nil {
			t.Fatalf("%s: no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s did not quit", k)
		}
	}
	// ctrl+c quits from chat too; plain q types into the chat.
	m = press(t, m, "l")
	if m.Focus() != FocusChat {
		t.Fatal("l should focus chat")
	}
	m = press(t, m, "q")
	if m.chat.Value() != "q" {
		t.Errorf("q in chat = %q", m.chat.Value())
	}
	if _, cmd := m.Update(keyMsg("ctrl+c")); cmd == nil {
		t.Error("ctrl+c must quit from chat")
	}
}

func TestAppLeadChatShortcutAndEsc(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "l")
	if m.Focus() != FocusChat {
		t.Fatal("l should focus chat")
	}
	m = typeText(t, m, "hi lead")
	m = press(t, m, "enter")
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "chat:s1:hi lead" {
		t.Errorf("calls = %v", calls)
	}
	msgs := m.chat.Messages()
	if len(msgs) != 2 || msgs[1].Text != "echo hi lead" {
		t.Errorf("messages = %+v", msgs)
	}
	m = press(t, m, "esc")
	if m.Focus() != FocusTasks {
		t.Errorf("esc: focus = %s", m.Focus())
	}
}

func TestAppChatMultilineThroughRoot(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "l")
	m = typeText(t, m, "a")
	m = press(t, m, "ctrl+j")
	m = typeText(t, m, "b")
	m = press(t, m, "enter")
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "chat:s1:a\nb" {
		t.Errorf("calls = %q", calls)
	}
}

func TestAppDelegate(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "tab") // tasks, selected t1 (queued)
	m = press(t, m, "d")
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "delegate:t1" {
		t.Errorf("calls = %v", calls)
	}
	if !strings.Contains(m.status, "delegated") || m.statusErr {
		t.Errorf("status = %q err=%v", m.status, m.statusErr)
	}
	// Non-queued tasks are refused without calling the backend.
	m = press(t, m, "down")
	m = press(t, m, "down") // t2, working
	m = press(t, m, "d")
	if !m.statusErr || !strings.Contains(m.status, "only queued") {
		t.Errorf("status = %q", m.status)
	}
	if len(fb.Calls()) != 1 {
		t.Errorf("unexpected backend calls: %v", fb.Calls())
	}
}

func TestAppNewTaskFlow(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "n")
	if m.overlay != overlayTitle {
		t.Fatal("n should open the title prompt")
	}
	m = press(t, m, "enter") // empty title rejected
	if m.overlay != overlayTitle || !m.statusErr {
		t.Error("empty title must be rejected")
	}
	m = typeText(t, m, "Ship it")
	m = press(t, m, "enter")
	if m.overlay != overlayBrief {
		t.Fatal("expected brief prompt")
	}
	if !strings.Contains(ansi.Strip(m.View()), "Brief for") {
		t.Error("prompt not rendered")
	}
	m = typeText(t, m, "do the thing")
	m = press(t, m, "enter")
	if m.overlay != overlayNone {
		t.Error("prompt should close")
	}
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "create:s1:Ship it:do the thing" {
		t.Errorf("calls = %v", calls)
	}
	if m.tasks.SelectedID() != "new-1" {
		t.Errorf("new task not selected: %q", m.tasks.SelectedID())
	}
}

func TestAppNewTaskDefaultsBriefAndCancel(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "n")
	m = typeText(t, m, "Only title")
	m = press(t, m, "enter")
	m = press(t, m, "enter")
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "create:s1:Only title:Only title" {
		t.Errorf("calls = %v", calls)
	}
	m = press(t, m, "n")
	m = typeText(t, m, "nope")
	m = press(t, m, "esc")
	if m.overlay != overlayNone || len(fb.Calls()) != 1 {
		t.Error("esc must cancel without creating")
	}
	// Keys typed in the prompt must not trigger global shortcuts.
	m = press(t, m, "n")
	m = typeText(t, m, "qdl")
	if m.input.Value() != "qdl" || m.Focus() != FocusSpaces {
		t.Errorf("prompt leaked keys: %q focus=%s", m.input.Value(), m.Focus())
	}
}

func TestAppApproveRequiresConfirmation(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "tab")
	m = press(t, m, "down")
	m = press(t, m, "down")
	m = press(t, m, "down") // t3, review
	m = press(t, m, "a")
	if m.overlay != overlayConfirm {
		t.Fatal("a should ask for confirmation")
	}
	if s := ansi.Strip(m.View()); !strings.Contains(s, "Reviewer requested changes") || !strings.Contains(s, "Refactor API") {
		t.Errorf("confirm text missing context:\n%s", s)
	}
	m = press(t, m, "n")
	if len(fb.Calls()) != 0 || m.overlay != overlayNone {
		t.Errorf("declined: calls=%v overlay=%v", fb.Calls(), m.overlay)
	}
	m = press(t, m, "a")
	m = press(t, m, "y")
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "approve:t3" {
		t.Errorf("calls = %v", calls)
	}
	if !strings.Contains(m.status, "approved") {
		t.Errorf("status = %q", m.status)
	}
}

func TestAppApproveRefusedOutsideReview(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "tab")
	m = press(t, m, "a") // t1 is queued
	if m.overlay != overlayNone || !m.statusErr || len(fb.Calls()) != 0 {
		t.Errorf("overlay=%v status=%q calls=%v", m.overlay, m.status, fb.Calls())
	}
}

func TestAppReview(t *testing.T) {
	m, fb := loadedApp(t)
	m = press(t, m, "tab")
	m = press(t, m, "down")
	m = press(t, m, "down") // working: t2
	m = press(t, m, "r")
	if calls := fb.Calls(); len(calls) != 1 || calls[0] != "review:t2" {
		t.Errorf("calls = %v", calls)
	}
	// In review already: jump to the verdict.
	m = press(t, m, "down")
	m = press(t, m, "r")
	if m.Focus() != FocusInspector || m.inspect.Tab() != TabReview {
		t.Errorf("focus=%s tab=%d", m.Focus(), m.inspect.Tab())
	}
	// Queued tasks cannot be reviewed.
	m = press(t, m, "2") // back to tasks
	m = press(t, m, "up")
	m = press(t, m, "up")
	m = press(t, m, "up")
	m = press(t, m, "r")
	if !m.statusErr {
		t.Errorf("expected refusal, status = %q", m.status)
	}
}

func TestAppWorktree(t *testing.T) {
	m, _ := loadedApp(t)
	m = press(t, m, "tab")
	m = press(t, m, "w") // t1 has no worktree
	if !m.statusErr || m.Focus() != FocusTasks {
		t.Errorf("status=%q focus=%s", m.status, m.Focus())
	}
	m = press(t, m, "down")
	m = press(t, m, "down") // t2
	m = press(t, m, "w")
	if m.Focus() != FocusInspector || m.inspect.Tab() != TabDiff || !strings.Contains(m.status, "/tmp/wt/t2") {
		t.Errorf("focus=%s tab=%d status=%q", m.Focus(), m.inspect.Tab(), m.status)
	}
}

func TestAppActionErrorIsShown(t *testing.T) {
	m, fb := loadedApp(t)
	fb.failAct = errBoom
	m = press(t, m, "tab")
	m = press(t, m, "d")
	if !m.statusErr || !strings.Contains(m.status, "boom") {
		t.Errorf("status = %q", m.status)
	}
	if !strings.Contains(ansi.Strip(m.View()), "boom") {
		t.Error("error not rendered")
	}
}

func TestAppViewToggleAndHelp(t *testing.T) {
	m, _ := loadedApp(t)
	m = press(t, m, "v")
	if m.tasks.Mode() != ViewBacklog {
		t.Error("v should switch to backlog")
	}
	m = press(t, m, "v")
	if m.tasks.Mode() != ViewLanes {
		t.Error("v should switch back")
	}
	m = press(t, m, "?")
	if !m.showHelp || !strings.Contains(ansi.Strip(m.View()), "approve/merge") {
		t.Error("full help not shown")
	}
	m = press(t, m, "esc")
	if m.showHelp {
		t.Error("esc should close help")
	}
}

func TestAppEventUpdatesInspectorAndProgress(t *testing.T) {
	m, _ := loadedApp(t)
	m = press(t, m, "tab")
	m = press(t, m, "down")
	m = press(t, m, "down") // t2
	spend := 0.1
	nm, _ := m.Update(EventMsg{Event: schema.AgentEvent{
		Type: schema.EventTaskProgress, TaskID: "t2", SpaceID: "s1", Role: schema.RoleWorker,
		Timestamp: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), Message: "fixed", SpendUSD: &spend,
	}})
	m = nm.(Model)
	m.inspect.SetTab(TabLogs)
	if v := ansi.Strip(m.inspect.View()); !strings.Contains(v, "12:00:00 worker task_progress:") || !strings.Contains(v, "fixed") {
		t.Errorf("event not in logs:\n%s", ansi.Strip(m.inspect.View()))
	}
	nm, _ = m.Update(EventMsg{Event: schema.AgentEvent{
		Type: schema.EventReviewVerdict, TaskID: "t2", SpaceID: "s1", Role: schema.RoleReviewer,
		Timestamp: time.Now(), Verdict: &schema.ReviewVerdict{Verdict: schema.DecisionApproved,
			Checklist: []schema.ChecklistItem{{Item: "no stale reads", Met: true}}},
	}})
	m = nm.(Model)
	if got := m.tasks.progress["t2"]; got != (Progress{Met: 1, Total: 1}) {
		t.Errorf("progress = %+v", got)
	}
}

func TestAppStaleTasksIgnored(t *testing.T) {
	m, _ := loadedApp(t)
	nm, cmd := m.Update(TasksMsg{SpaceID: "s2", Tasks: []schema.Task{{ID: "zz", SpaceID: "s2", Title: "x", Status: schema.StatusQueued}}})
	m = nm.(Model)
	if cmd != nil || len(m.tasks.Tasks()) != 5 {
		t.Error("tasks for a non-selected space must be ignored")
	}
}

func TestAppLoadErrorsSurface(t *testing.T) {
	m, _ := loadedApp(t)
	nm, _ := m.Update(SpacesMsg{Err: errBoom})
	m = nm.(Model)
	if !m.statusErr || !strings.Contains(m.status, "spaces: boom") || m.refreshing {
		t.Errorf("status=%q refreshing=%v", m.status, m.refreshing)
	}
}

func TestAppTickRefreshesOnce(t *testing.T) {
	fb := newFake()
	m := New(Options{Backend: fb, RefreshInterval: time.Millisecond})
	m = resized(t, m, 140, 40)
	nm, cmd := m.Update(tickMsg(time.Now()))
	m = nm.(Model)
	if cmd == nil || !m.refreshing {
		t.Fatal("tick should start a refresh")
	}
	// While a refresh is in flight a tick must not start another chain.
	nm, cmd = m.Update(tickMsg(time.Now()))
	if msg, ok := runCmd(cmd, 50*time.Millisecond); ok {
		if b, isBatch := msg.(tea.BatchMsg); isBatch && len(b) > 1 {
			t.Error("overlapping refresh started")
		}
	}
	_ = nm
}

func TestAppNilBackendAndContext(t *testing.T) {
	m := New(Options{RefreshInterval: -1})
	m = resized(t, m, 100, 30)
	m = settle(t, m, m.Init())
	if !strings.Contains(ansi.Strip(m.View()), "No spaces") {
		t.Error("empty state missing")
	}
	m = press(t, m, "n")
	if !m.statusErr {
		t.Error("n without a space should be refused")
	}
	if cmd := m.Init(); cmd == nil {
		t.Error("init must load spaces")
	}
	_ = context.Canceled
}

func TestFocusString(t *testing.T) {
	for f, want := range map[Focus]string{FocusSpaces: "spaces", FocusTasks: "tasks", FocusInspector: "inspector", FocusChat: "chat", focusCount: "unknown"} {
		if f.String() != want {
			t.Errorf("%d = %q", f, f.String())
		}
	}
}

func TestAppLayoutAgentWindowFullHeight(t *testing.T) {
	m, _ := loadedApp(t)
	l := m.layout()
	if l.spacesH < 7 || l.spacesH > 10 || l.spacesH+l.tasksH != l.bodyH {
		t.Fatalf("left column heights wrong: %+v", l)
	}
	if l.centerW < l.leftW || l.centerW < l.inspW {
		t.Errorf("center column should be the widest: %+v", l)
	}
	if m.chat.height != l.bodyH-3 || m.chat.width != l.centerW-4 {
		t.Errorf("chat size = %dx%d, layout %+v", m.chat.width, m.chat.height, l)
	}
	if m.inspect.height != l.bodyH-3 {
		t.Errorf("inspector height = %d, want %d", m.inspect.height, l.bodyH-3)
	}
	if m.spaces.height != l.spacesH-3 || m.tasks.height != l.tasksH-3 || m.tasks.width != l.leftW-4 {
		t.Errorf("left panes mis-sized: spaces h=%d tasks %dx%d", m.spaces.height, m.tasks.width, m.tasks.height)
	}
}

func TestAppQuickFocusKeys(t *testing.T) {
	m, _ := loadedApp(t)
	for _, tc := range []struct {
		key  string
		want Focus
	}{{"2", FocusTasks}, {"4", FocusInspector}, {"1", FocusSpaces}, {"3", FocusChat}} {
		m = press(t, m, tc.key)
		if m.Focus() != tc.want {
			t.Fatalf("%s: focus = %s, want %s", tc.key, m.Focus(), tc.want)
		}
	}
	if !m.chat.Focused() {
		t.Error("3 should focus the chat input")
	}
	m = press(t, m, "1") // typed into the chat, not a quick key
	if m.Focus() != FocusChat || m.chat.Value() != "1" {
		t.Errorf("focus=%s value=%q", m.Focus(), m.chat.Value())
	}
	m = press(t, m, "esc") // draft present: esc keeps focus
	if m.Focus() != FocusChat {
		t.Errorf("esc with draft left chat: %s", m.Focus())
	}
	m = press(t, m, "backspace")
	m = press(t, m, "esc")
	if m.Focus() != FocusTasks || m.chat.Focused() {
		t.Errorf("esc on empty chat: focus=%s focused=%v", m.Focus(), m.chat.Focused())
	}
	m = press(t, m, "l")
	if m.Focus() != FocusChat {
		t.Errorf("l: focus = %s", m.Focus())
	}
}

func TestAppRestoresChatHistory(t *testing.T) {
	fb := newFake()
	at := time.Date(2025, 1, 2, 3, 4, 0, 0, time.UTC)
	fb.history = map[string][]ChatMessage{
		"s1": {{Role: ChatUser, Text: "hello", At: at}, {Role: ChatLead, Text: "welcome back", At: at}},
		"s2": {{Role: ChatLead, Text: "beta history", At: at}},
	}
	m := New(Options{Backend: fb, RefreshInterval: -1})
	m = resized(t, m, 140, 40)
	m = settle(t, m, m.Init())
	if got := m.chat.Messages(); len(got) != 2 || got[1].Text != "welcome back" {
		t.Fatalf("s1 history = %+v", got)
	}
	m = press(t, m, "down")
	if got := m.chat.Messages(); len(got) != 1 || got[0].Text != "beta history" {
		t.Fatalf("s2 history = %+v", got)
	}
	// Refreshes must not reload what was restored already.
	fb.mu.Lock()
	n := len(fb.histCalls)
	fb.mu.Unlock()
	if n != 2 {
		t.Errorf("history calls = %d, want 2", n)
	}
	m = settle(t, m, m.startRefresh())
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.histCalls) != n {
		t.Errorf("refresh reloaded history: %v", fb.histCalls)
	}
}

func TestAppChatHistoryErrorShown(t *testing.T) {
	fb := newFake()
	fb.histErr = errors.New("disk gone")
	m := New(Options{Backend: fb, RefreshInterval: -1})
	m = resized(t, m, 140, 40)
	m = settle(t, m, m.Init())
	if !m.statusErr || !strings.Contains(m.status, "history: disk gone") {
		t.Errorf("status = %q", m.status)
	}
}
