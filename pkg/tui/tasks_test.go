package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"fleet/pkg/schema"
)

func newTasks(w, h int) *TasksModel {
	m := NewTasks(NewStyles())
	m.SetSize(w, h)
	m.SetTasks(sampleTasks())
	return m
}

func TestTasksGroupedByLane(t *testing.T) {
	m := newTasks(100, 20)
	var ids []string
	for _, tk := range m.Tasks() {
		ids = append(ids, tk.ID)
	}
	if got := strings.Join(ids, ","); got != "t1,t5,t2,t3,t4" {
		t.Errorf("order = %s", got)
	}
	if c := m.Counts(); c != [4]int{2, 1, 1, 1} {
		t.Errorf("counts = %v", c)
	}
}

func TestTasksClosedShownInDone(t *testing.T) {
	m := NewTasks(NewStyles())
	m.SetSize(100, 20)
	m.SetTasks([]schema.Task{{ID: "c", SpaceID: "s", Title: "closed one", Status: schema.StatusClosed}})
	if m.Counts()[3] != 1 {
		t.Errorf("closed not in done lane: %v", m.Counts())
	}
	if !strings.Contains(ansi.Strip(m.View()), "✕ closed one") {
		t.Error("closed marker missing")
	}
}

func TestTasksLaneNavigation(t *testing.T) {
	m := newTasks(100, 30)
	if m.SelectedID() != "t1" {
		t.Fatalf("initial = %s", m.SelectedID())
	}
	m.Update(keyMsg("down"))
	if m.SelectedID() != "t5" {
		t.Errorf("down = %s", m.SelectedID())
	}
	m.Update(keyMsg("down")) // stays inside the lane
	if m.SelectedID() != "t5" {
		t.Errorf("lane clamp = %s", m.SelectedID())
	}
	m.Update(keyMsg("right")) // row 1 clamps to the single working task
	if m.SelectedID() != "t2" {
		t.Errorf("right = %s", m.SelectedID())
	}
	m.Update(keyMsg("right"))
	m.Update(keyMsg("right"))
	m.Update(keyMsg("right")) // last lane
	if m.SelectedID() != "t4" {
		t.Errorf("last lane = %s", m.SelectedID())
	}
	m.Update(keyMsg("h"))
	if m.SelectedID() != "t3" {
		t.Errorf("left = %s", m.SelectedID())
	}
}

func TestTasksSkipEmptyLane(t *testing.T) {
	m := NewTasks(NewStyles())
	m.SetSize(100, 30)
	ts := sampleTasks()
	m.SetTasks([]schema.Task{ts[0], ts[3]}) // queued + done, nothing between
	m.Update(keyMsg("right"))
	if m.SelectedID() != "t4" {
		t.Errorf("did not skip empty lanes: %s", m.SelectedID())
	}
}

func TestTasksBacklogNavigation(t *testing.T) {
	m := newTasks(100, 30)
	m.SetMode(ViewBacklog)
	for i := 0; i < 3; i++ {
		m.Update(keyMsg("j"))
	}
	if m.SelectedID() != "t3" {
		t.Errorf("backlog j*3 = %s", m.SelectedID())
	}
	m.Update(keyMsg("right")) // no-op in backlog
	if m.SelectedID() != "t3" {
		t.Errorf("right moved in backlog: %s", m.SelectedID())
	}
	m.Update(keyMsg("G"))
	if m.SelectedID() != "t4" {
		t.Errorf("G = %s", m.SelectedID())
	}
}

func TestTasksSelectionSurvivesRefresh(t *testing.T) {
	m := newTasks(100, 30)
	m.SelectByID("t3")
	ts := sampleTasks()
	ts[2].Status = schema.StatusDone // moves lanes
	m.SetTasks(append([]schema.Task{{ID: "t0", SpaceID: "s1", Title: "first", Status: schema.StatusQueued}}, ts...))
	if m.SelectedID() != "t3" {
		t.Errorf("selection = %s", m.SelectedID())
	}
	m.SetTasks(nil)
	if _, ok := m.Selected(); ok {
		t.Error("expected no selection")
	}
	if !strings.Contains(ansi.Strip(m.View()), "No tasks") {
		t.Error("empty state missing")
	}
}

func TestTasksViewContent(t *testing.T) {
	m := newTasks(120, 20)
	m.SetProgress("t3", Progress{Met: 1, Total: 2})
	v := ansi.Strip(m.View())
	for _, want := range []string{"QUEUED", "WORKING", "REVIEW", "DONE", "Add login", "@w1", "⎇ fleet/t2", "$0.42", "☐ AC 2", "◐ AC 1/2", "⚠ no AC", "no branch", "no worker"} {
		if !strings.Contains(v, want) {
			t.Errorf("lanes missing %q:\n%s", want, v)
		}
	}
	assertWithin(t, m.View(), 120, 20)
}

func TestTasksBacklogViewAndFallback(t *testing.T) {
	m := newTasks(100, 20)
	m.SetMode(ViewBacklog)
	v := ansi.Strip(m.View())
	for _, want := range []string{" WORKING ", " REVIEW ", "Fix cache bug", "@w2 · ⎇ fleet/t3"} {
		if !strings.Contains(v, want) {
			t.Errorf("backlog missing %q:\n%s", want, v)
		}
	}
	// Lanes mode on a narrow pane silently falls back to the backlog.
	n := newTasks(40, 20)
	if n.Mode() != ViewLanes || !strings.Contains(ansi.Strip(n.View()), " QUEUED ") {
		t.Errorf("narrow fallback failed:\n%s", ansi.Strip(n.View()))
	}
	assertWithin(t, n.View(), 40, 20)
}

func TestTasksScrollKeepsSelectionVisible(t *testing.T) {
	var ts []schema.Task
	for i := 0; i < 20; i++ {
		ts = append(ts, schema.Task{ID: string(rune('a' + i)), SpaceID: "s", Title: "task-" + string(rune('a'+i)), Status: schema.StatusQueued})
	}
	for _, mode := range []ViewMode{ViewLanes, ViewBacklog} {
		m := NewTasks(NewStyles())
		m.SetSize(100, 12)
		m.SetMode(mode)
		m.SetTasks(ts)
		for i := 0; i < 19; i++ {
			m.Update(keyMsg("j"))
		}
		if v := ansi.Strip(m.View()); !strings.Contains(v, "task-t") {
			t.Errorf("mode %d: selected task not visible:\n%s", mode, v)
		}
		assertWithin(t, m.View(), 100, 12)
	}
}
