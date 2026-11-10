package agent

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderLeadPrompt(t *testing.T) {
	got, err := RenderLeadPrompt(LeadPromptData{Space: "web", Summary: "  The web app. ", Dir: "/src/web"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`space "web"`, "Space summary: The web app.", "Working directory: /src/web", "fleet-lead skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	min, err := RenderLeadPrompt(LeadPromptData{Space: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(min, "summary") || strings.Contains(min, "Working directory") {
		t.Errorf("optional sections leaked:\n%s", min)
	}
	if _, err := RenderLeadPrompt(LeadPromptData{Space: "../x"}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("err = %v", err)
	}
}

func TestRenderWorkerPrompt(t *testing.T) {
	got, err := RenderWorkerPrompt(WorkerPromptData{
		Space: "web", Task: "t1", Title: "Fix login", Brief: "\nFix the bug.\n",
		Branch: "fix/login", BaseBranch: "main", Worktree: "/wt",
		Acceptance: []string{"login works", "tests pass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Task t1: Fix login", "Space: web", "Branch: fix/login (base: main)", "Worktree: /wt",
		"## Brief\n\nFix the bug.\n", "1. login works", "2. tests pass", "AGENTS.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	min, err := RenderWorkerPrompt(WorkerPromptData{Space: "web", Task: "t1", Brief: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Branch:", "Worktree:", "Acceptance criteria", ": \n"} {
		if strings.Contains(min, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, min)
		}
	}
	if !strings.HasPrefix(min, "# Task t1\n") {
		t.Errorf("header = %q", strings.SplitN(min, "\n", 2)[0])
	}
}

func TestRenderWorkerPromptErrors(t *testing.T) {
	if _, err := RenderWorkerPrompt(WorkerPromptData{Space: "s", Task: "t", Brief: "  "}); !errors.Is(err, ErrInvalidPrompt) {
		t.Errorf("empty brief: %v", err)
	}
	if _, err := RenderWorkerPrompt(WorkerPromptData{Space: "s", Task: "a/b", Brief: "x"}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("bad task: %v", err)
	}
	if _, err := RenderWorkerPrompt(WorkerPromptData{Space: "", Task: "t", Brief: "x"}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("bad space: %v", err)
	}
}

func TestRenderReviewerPrompt(t *testing.T) {
	diff := "diff --git a/x b/x\n+added {braces} and {{template}}\n"
	got, err := RenderReviewerPrompt(ReviewPromptData{
		Space: "web", Task: "t1", Title: "Fix", Criteria: []string{" a ", "", "b"}, Diff: diff,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Review of task t1: Fix", "1. a", "2. b", diff, `"verdict"`, "```diff", "```json"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "truncated") {
		t.Error("untruncated diff flagged as truncated")
	}
	if strings.Contains(got, "3. ") {
		t.Error("blank criterion was numbered")
	}
}

func TestRenderReviewerPromptTruncates(t *testing.T) {
	diff := strings.Repeat("+line\n", 100)
	got, err := RenderReviewerPrompt(ReviewPromptData{Space: "s", Task: "t", Criteria: []string{"c"}, Diff: diff}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation not flagged")
	}
	if strings.Count(got, "+line") > 10 {
		t.Error("diff not truncated")
	}
}

func TestRenderReviewerPromptErrors(t *testing.T) {
	ok := ReviewPromptData{Space: "s", Task: "t", Criteria: []string{"c"}, Diff: "d"}
	if _, err := RenderReviewerPrompt(ok, 0); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*ReviewPromptData){
		"no criteria": func(d *ReviewPromptData) { d.Criteria = []string{" ", ""} },
		"empty diff":  func(d *ReviewPromptData) { d.Diff = "  \n" },
		"bad task":    func(d *ReviewPromptData) { d.Task = ".." },
	} {
		d := ok
		mut(&d)
		if _, err := RenderReviewerPrompt(d, 0); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestTruncateDiff(t *testing.T) {
	if got, trunc := TruncateDiff("abc", 0); got != "abc" || trunc {
		t.Errorf("max=0: %q %v", got, trunc)
	}
	if got, trunc := TruncateDiff("abc", 3); got != "abc" || trunc {
		t.Errorf("exact fit: %q %v", got, trunc)
	}
	if got, trunc := TruncateDiff("one\ntwo\nthree", 10); got != "one\ntwo" || !trunc {
		t.Errorf("line boundary: %q %v", got, trunc)
	}
	// No newline: must not split a multi-byte rune.
	s := strings.Repeat("é", 10) // 20 bytes
	got, trunc := TruncateDiff(s, 5)
	if !trunc || !utf8.ValidString(got) || got != "éé" {
		t.Errorf("rune safety: %q %v", got, trunc)
	}
}
