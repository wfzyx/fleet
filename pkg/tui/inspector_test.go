package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"fleet/pkg/schema"
)

func newInspector(t *testing.T) *InspectorModel {
	t.Helper()
	m := NewInspector(NewStyles())
	m.SetSize(60, 20)
	tk := sampleTasks()[2]
	tk.Brief = "Refactor the API layer.\nKeep behaviour identical."
	m.SetTask(tk, true)
	m.SetDetail("t3", newFake().details["t3"], nil)
	return m
}

func TestInspectorOverview(t *testing.T) {
	m := newInspector(t)
	v := ansi.Strip(m.View())
	for _, want := range []string{"Refactor API", "REVIEW", "Brief", "Refactor the API layer.", "Acceptance criteria",
		"☑ tests pass", "✗ docs updated", "Worktree", "/tmp/wt/t3", "fleet/t3", "w2"} {
		if !strings.Contains(v, want) {
			t.Errorf("overview missing %q:\n%s", want, v)
		}
	}
	assertBox(t, m.View(), 60, 20)
}

func TestInspectorDiffTab(t *testing.T) {
	m := newInspector(t)
	m.SetTab(TabDiff)
	v := m.View()
	plain := ansi.Strip(v)
	for _, want := range []string{"+new", "-old", "@@ -1 +1 @@"} {
		if !strings.Contains(plain, want) {
			t.Errorf("diff missing %q:\n%s", want, plain)
		}
	}
	// Added and removed lines are styled with different sequences.
	add := m.styleDiffLine("+x")
	del := m.styleDiffLine("-x")
	if add == "+x" || del == "-x" {
		t.Skip("colour profile disabled in this environment")
	}
	if add == del {
		t.Error("add/del lines must be styled differently")
	}
}

func TestInspectorDiffEmptyAndLoading(t *testing.T) {
	m := NewInspector(NewStyles())
	m.SetSize(40, 10)
	if !strings.Contains(ansi.Strip(m.View()), "Select a task") {
		t.Error("no-task placeholder missing")
	}
	m.SetTask(sampleTasks()[1], true)
	m.SetTab(TabDiff)
	if !strings.Contains(ansi.Strip(m.View()), "Loading diff") {
		t.Error("loading placeholder missing")
	}
	m.SetDetail("t2", TaskDetail{}, nil)
	if !strings.Contains(ansi.Strip(m.View()), "No changes") {
		t.Error("empty diff placeholder missing")
	}
}

func TestInspectorLogsMergeLiveEvents(t *testing.T) {
	m := newInspector(t)
	m.SetTab(TabLogs)
	m.AppendLog("t3", "12:00:01 worker task_progress: tests green")
	m.AppendLog("other", "must be ignored")
	v := ansi.Strip(m.View())
	for _, want := range []string{"turn 1: read files", "turn 2: edit", "tests green"} {
		if !strings.Contains(v, want) {
			t.Errorf("logs missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "must be ignored") {
		t.Error("log for another task leaked")
	}
	// A fresh load supersedes live lines.
	m.SetDetail("t3", TaskDetail{Logs: []string{"fresh"}}, nil)
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "fresh") || strings.Contains(v, "tests green") {
		t.Errorf("detail reload did not supersede live lines:\n%s", v)
	}
}

func TestInspectorReviewTab(t *testing.T) {
	m := newInspector(t)
	m.SetTab(TabReview)
	v := ansi.Strip(m.View())
	for _, want := range []string{"CHANGES REQUESTED", "1/2 criteria met", "☑ tests pass", "✗ docs updated", "README missing", "please fix docs"} {
		if !strings.Contains(v, want) {
			t.Errorf("review missing %q:\n%s", want, v)
		}
	}
	m.SetVerdict("t3", &schema.ReviewVerdict{Verdict: schema.DecisionApproved})
	if !strings.Contains(ansi.Strip(m.View()), "APPROVED") {
		t.Error("live verdict not shown")
	}
	m.SetVerdict("t3", nil)
	if !strings.Contains(ansi.Strip(m.View()), "No reviewer verdict") {
		t.Error("missing-verdict placeholder not shown")
	}
}

func TestInspectorTabNavigation(t *testing.T) {
	m := newInspector(t)
	m.Update(keyMsg("right"))
	if m.Tab() != TabDiff {
		t.Errorf("right = %d", m.Tab())
	}
	m.Update(keyMsg("]"))
	m.Update(keyMsg("]"))
	if m.Tab() != TabReview {
		t.Errorf("] = %d", m.Tab())
	}
	m.Update(keyMsg("right")) // wraps
	if m.Tab() != TabOverview {
		t.Errorf("wrap = %d", m.Tab())
	}
	m.Update(keyMsg("left")) // wraps back
	if m.Tab() != TabReview {
		t.Errorf("left wrap = %d", m.Tab())
	}
}

func TestInspectorTaskSwitchResets(t *testing.T) {
	m := newInspector(t)
	m.SetDetail("stale", TaskDetail{Logs: []string{"x"}}, nil) // wrong ID: ignored
	m.SetTask(sampleTasks()[1], true)
	if m.Verdict() != nil {
		t.Error("detail must reset on task change")
	}
	m.SetTask(sampleTasks()[1], true) // same task: detail kept
	m.SetDetail("t2", TaskDetail{Logs: []string{"keep"}}, nil)
	m.SetTask(sampleTasks()[1], true)
	m.SetTab(TabLogs)
	if !strings.Contains(ansi.Strip(m.View()), "keep") {
		t.Error("refreshing the same task dropped its detail")
	}
	m.SetTask(schema.Task{}, false)
	if m.TaskID() != "" {
		t.Error("TaskID should be empty without a task")
	}
}

func TestInspectorShowsLoadError(t *testing.T) {
	m := newInspector(t)
	m.SetDetail("t3", TaskDetail{}, errors.New("git exploded"))
	if !strings.Contains(ansi.Strip(m.View()), "git exploded") {
		t.Error("load error not shown")
	}
}

func TestInspectorScrollAndSanitize(t *testing.T) {
	m := NewInspector(NewStyles())
	m.SetSize(40, 8)
	m.SetTask(sampleTasks()[1], true)
	var diff strings.Builder
	for i := 0; i < 100; i++ {
		diff.WriteString("+line\x1b[31m with escape\tand tab\n")
	}
	m.SetDetail("t2", TaskDetail{Diff: diff.String()}, nil)
	m.SetTab(TabDiff)
	before := ansi.Strip(m.View())
	m.Update(keyMsg("pgdown"))
	if ansi.Strip(m.View()) == before && m.vp.TotalLineCount() > m.vp.Height {
		// identical text is possible; offset must still have moved
		if m.vp.YOffset == 0 {
			t.Error("pgdown did not scroll")
		}
	}
	if strings.Contains(m.View(), "\t") {
		t.Error("tab leaked into the view")
	}
	assertBox(t, m.View(), 40, 8)
}
