package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validTask() Task {
	return Task{
		ID: "t1", SpaceID: "s1", Title: "Fix bug", Brief: "b",
		AcceptanceCriteria: []string{"tests pass"}, Status: StatusWorking, AssignedWorker: "w1",
		Branch: "fleet/t1", WorktreePath: "/x", PRUrl: "https://example.test/pr/1", JiraKey: "ABC-1", SpendUSD: 0.25,
	}
}

func TestStatusTransitions(t *testing.T) {
	ok := [][2]Status{
		{StatusQueued, StatusWorking}, {StatusWorking, StatusReview}, {StatusReview, StatusDone},
		{StatusReview, StatusWorking}, {StatusWorking, StatusClosed}, {StatusQueued, StatusClosed},
	}
	for _, c := range ok {
		if !c[0].CanTransition(c[1]) {
			t.Errorf("%s -> %s should be allowed", c[0], c[1])
		}
	}
	bad := [][2]Status{{StatusQueued, StatusDone}, {StatusDone, StatusWorking}, {StatusClosed, StatusQueued}, {StatusReview, StatusQueued}}
	for _, c := range bad {
		if c[0].CanTransition(c[1]) {
			t.Errorf("%s -> %s should be refused", c[0], c[1])
		}
	}
	if !StatusDone.Terminal() || !StatusClosed.Terminal() || StatusWorking.Terminal() {
		t.Fatal("terminal wrong")
	}
	for _, s := range Statuses() {
		if !s.Valid() {
			t.Fatal(s)
		}
	}
	if Status("x").Valid() {
		t.Fatal("x valid")
	}
}

func TestTaskRoundTripFieldNames(t *testing.T) {
	data, err := MarshalTask(validTask())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "space_id", "title", "brief", "acceptance_criteria", "status",
		"assigned_worker", "branch", "worktree_path", "pr_url", "jira_key", "spend_usd"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
	got, err := UnmarshalTask(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t1" || got.Status != StatusWorking || got.SpendUSD != 0.25 || got.AcceptanceCriteria[0] != "tests pass" {
		t.Fatalf("%+v", got)
	}
}

func TestMarshalTaskNilCriteriaIsEmptyArray(t *testing.T) {
	tk := Task{ID: "t", SpaceID: "s", Title: "x", Status: StatusQueued}
	data, err := MarshalTask(tk)
	if err != nil || !strings.Contains(string(data), `"acceptance_criteria":[]`) {
		t.Fatalf("%s %v", data, err)
	}
}

func TestTaskValidate(t *testing.T) {
	mut := map[string]func(*Task){
		"no id":             func(t *Task) { t.ID = "" },
		"no space":          func(t *Task) { t.SpaceID = "" },
		"no title":          func(t *Task) { t.Title = "" },
		"bad status":        func(t *Task) { t.Status = "nope" },
		"neg spend":         func(t *Task) { t.SpendUSD = -1 },
		"empty criterion":   func(t *Task) { t.AcceptanceCriteria = []string{""} },
		"working no worker": func(t *Task) { t.AssignedWorker = "" },
	}
	for name, f := range mut {
		tk := validTask()
		f(&tk)
		if err := tk.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	q := Task{ID: "t", SpaceID: "s", Title: "x", Status: StatusQueued}
	if err := q.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUnmarshalTaskStrict(t *testing.T) {
	good := `{"id":"t","space_id":"s","title":"x","brief":"","acceptance_criteria":[],"status":"queued","spend_usd":0}`
	if _, err := UnmarshalTask([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"unknown field": `{"id":"t","space_id":"s","title":"x","status":"queued","extra":1}`,
		"bad status":    `{"id":"t","space_id":"s","title":"x","status":"paused"}`,
		"wrong type":    `{"id":1,"space_id":"s","title":"x","status":"queued"}`,
		"trailing":      good + `{}`,
		"null":          `null`,
		"empty":         ``,
		"missing":       `{"status":"queued"}`,
	}
	for name, body := range bad {
		if _, err := UnmarshalTask([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestAgentRole(t *testing.T) {
	for _, r := range []AgentRole{RoleLead, RoleWorker, RoleReviewer} {
		if !r.Valid() {
			t.Fatal(r)
		}
	}
	var r AgentRole
	if err := json.Unmarshal([]byte(`"boss"`), &r); err == nil {
		t.Fatal("expected error")
	}
	if err := json.Unmarshal([]byte(`"lead"`), &r); err != nil || r != RoleLead {
		t.Fatal(err)
	}
}

func ev(tp EventType, role AgentRole) AgentEvent {
	return AgentEvent{Type: tp, TaskID: "t1", SpaceID: "s1", Role: role, Timestamp: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func TestEventValidateValid(t *testing.T) {
	prog := ev(EventTaskProgress, RoleWorker)
	prog.Message = "50%"
	rev := ev(EventTaskReviewRequested, RoleWorker)
	rev.PRUrl = "https://example.test/pr/1"
	vd := ev(EventReviewVerdict, RoleReviewer)
	vd.Verdict = &ReviewVerdict{Verdict: DecisionApproved, Checklist: []ChecklistItem{{Item: "tests", Met: true}}}
	for _, e := range []AgentEvent{ev(EventTaskStarted, RoleWorker), prog, rev, vd, ev(EventTaskCompleted, RoleLead)} {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", e.Type, err)
		}
	}
}

func TestEventValidateInvalid(t *testing.T) {
	cases := map[string]func() AgentEvent{
		"bad type":        func() AgentEvent { return ev("x", RoleWorker) },
		"bad role":        func() AgentEvent { return ev(EventTaskStarted, "x") },
		"no task":         func() AgentEvent { e := ev(EventTaskStarted, RoleWorker); e.TaskID = ""; return e },
		"no space":        func() AgentEvent { e := ev(EventTaskStarted, RoleWorker); e.SpaceID = ""; return e },
		"no time":         func() AgentEvent { e := ev(EventTaskStarted, RoleWorker); e.Timestamp = time.Time{}; return e },
		"progress no msg": func() AgentEvent { return ev(EventTaskProgress, RoleWorker) },
		"review by lead":  func() AgentEvent { return ev(EventTaskReviewRequested, RoleLead) },
		"verdict missing": func() AgentEvent { return ev(EventReviewVerdict, RoleReviewer) },
		"verdict by worker": func() AgentEvent {
			e := ev(EventReviewVerdict, RoleWorker)
			e.Verdict = &ReviewVerdict{Verdict: DecisionApproved}
			return e
		},
		"verdict on started": func() AgentEvent {
			e := ev(EventTaskStarted, RoleWorker)
			e.Verdict = &ReviewVerdict{Verdict: DecisionApproved}
			return e
		},
		"pr on started": func() AgentEvent { e := ev(EventTaskStarted, RoleWorker); e.PRUrl = "u"; return e },
		"neg spend": func() AgentEvent {
			e := ev(EventTaskStarted, RoleWorker)
			v := -1.0
			e.SpendUSD = &v
			return e
		},
	}
	for name, f := range cases {
		if err := f().Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestReviewVerdictValidate(t *testing.T) {
	ok := []ReviewVerdict{
		{Verdict: DecisionApproved},
		{Verdict: DecisionApproved, Checklist: []ChecklistItem{{Item: "a", Met: true}}},
		{Verdict: DecisionChangesRequested, Notes: "fix it"},
		{Verdict: DecisionChangesRequested, Checklist: []ChecklistItem{{Item: "a", Met: false}}},
	}
	for i, v := range ok {
		if err := v.Validate(); err != nil {
			t.Errorf("%d: %v", i, err)
		}
	}
	bad := []ReviewVerdict{
		{Verdict: "maybe"},
		{Verdict: DecisionApproved, Checklist: []ChecklistItem{{Item: "a", Met: false}}},
		{Verdict: DecisionChangesRequested},
		{Verdict: DecisionApproved, Checklist: []ChecklistItem{{Item: "", Met: true}}},
	}
	for i, v := range bad {
		if err := v.Validate(); err == nil {
			t.Errorf("%d: expected error", i)
		}
	}
}

func TestEventRoundTrip(t *testing.T) {
	e := ev(EventReviewVerdict, RoleReviewer)
	e.Verdict = &ReviewVerdict{Verdict: DecisionChangesRequested, Checklist: []ChecklistItem{{Item: "lint", Met: false, Note: "fails"}}, Notes: "see lint"}
	data, err := MarshalEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict == nil || got.Verdict.Verdict != DecisionChangesRequested || got.Verdict.Checklist[0].Note != "fails" || !got.Timestamp.Equal(e.Timestamp) {
		t.Fatalf("%+v", got)
	}
	if _, err := MarshalEvent(AgentEvent{}); err == nil {
		t.Fatal("expected invalid marshal error")
	}
}

func TestUnmarshalEventStrict(t *testing.T) {
	base := `{"type":"task_started","task_id":"t","space_id":"s","role":"worker","timestamp":"2025-01-02T03:04:05Z"`
	if _, err := UnmarshalEvent([]byte(base + `}`)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"unknown field": base + `,"x":1}`,
		"bad type":      strings.Replace(base, "task_started", "task_exploded", 1) + `}`,
		"bad role":      strings.Replace(base, `"worker"`, `"intern"`, 1) + `}`,
		"bad verdict":   strings.Replace(base, "task_started", "review_verdict", 1) + `,"verdict":{"verdict":"meh","checklist":[]}}`,
		"bad time":      strings.Replace(base, "2025-01-02T03:04:05Z", "yesterday", 1) + `}`,
		"trailing":      base + `} x`,
	}
	for name, body := range bad {
		if _, err := UnmarshalEvent([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
