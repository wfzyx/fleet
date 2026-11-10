package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupPolicyDefaults(t *testing.T) {
	w := DefaultConfig(RoleWorker).Cleanup
	if !w.ShouldRemoveWorktree(OutcomeSucceeded) || !w.ShouldRemoveWorktree(OutcomeCancelled) {
		t.Error("worker worktree should be removed after success/cancel")
	}
	if w.ShouldRemoveWorktree(OutcomeFailed) || w.ShouldRemoveSession(OutcomeFailed) {
		t.Error("failed worker artifacts must be kept for inspection")
	}
	l := DefaultConfig(RoleLead).Cleanup
	if !l.IsZero() {
		t.Error("lead must never be cleaned up")
	}
	for _, o := range []Outcome{OutcomeSucceeded, OutcomeFailed, OutcomeCancelled} {
		if l.ShouldRemoveSession(o) || l.ShouldRemoveWorktree(o) {
			t.Errorf("lead cleaned after %s", o)
		}
	}
	r := DefaultConfig(RoleReviewer).Cleanup
	if r.ShouldRemoveWorktree(OutcomeSucceeded) {
		t.Error("reviewer must never remove the worktree")
	}
	if !r.ShouldRemoveSession(OutcomeSucceeded) || r.ShouldRemoveSession(OutcomeFailed) {
		t.Error("reviewer session removed on success only")
	}
}

func mkdirFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestCleanerApply(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	wt, _ := WorktreeDir("sp", "t1")
	sd, _ := SessionDir(RoleWorker, "sp", "t1")
	targets := Targets{Worktree: wt, SessionDir: sd}
	policy := DefaultConfig(RoleWorker).Cleanup

	// failed: nothing removed
	mkdirFile(t, wt)
	mkdirFile(t, sd)
	if err := (Cleaner{}).Apply(policy, OutcomeFailed, targets); err != nil {
		t.Fatal(err)
	}
	if !exists(wt) || !exists(sd) {
		t.Fatal("failed run artifacts were removed")
	}

	// succeeded: both removed
	if err := (Cleaner{}).Apply(policy, OutcomeSucceeded, targets); err != nil {
		t.Fatal(err)
	}
	if exists(wt) || exists(sd) {
		t.Fatal("succeeded run artifacts remain")
	}

	// cancelled: worktree only
	mkdirFile(t, wt)
	mkdirFile(t, sd)
	if err := (Cleaner{}).Apply(policy, OutcomeCancelled, targets); err != nil {
		t.Fatal(err)
	}
	if exists(wt) || !exists(sd) {
		t.Fatalf("cancelled: worktree exists=%v session exists=%v", exists(wt), exists(sd))
	}
}

func TestCleanerRefusesOutsidePaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	outside := t.TempDir()
	mkdirFile(t, outside)
	root, _ := WorktreeRoot()
	sroot, _ := SessionRoot()
	policy := CleanupPolicy{
		RemoveWorktree: []Outcome{OutcomeSucceeded},
		RemoveSession:  []Outcome{OutcomeSucceeded},
	}
	for _, tg := range []Targets{
		{Worktree: outside},
		{SessionDir: outside},
		{Worktree: root},
		{SessionDir: sroot},
		{Worktree: filepath.Join(root, "..", "..")},
	} {
		if err := (Cleaner{}).Apply(policy, OutcomeSucceeded, tg); err == nil {
			t.Errorf("Apply(%+v) should refuse", tg)
		}
	}
	if !exists(filepath.Join(outside, "f")) {
		t.Fatal("outside directory was deleted")
	}
}

func TestCleanerCustomRemoverAndErrorJoin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	wt, _ := WorktreeDir("sp", "t1")
	sd, _ := SessionDir(RoleWorker, "sp", "t1")
	mkdirFile(t, sd)
	boom := errors.New("boom")
	var got string
	c := Cleaner{RemoveWorktree: func(p string) error { got = p; return boom }}
	policy := CleanupPolicy{RemoveWorktree: []Outcome{OutcomeSucceeded}, RemoveSession: []Outcome{OutcomeSucceeded}}
	err := c.Apply(policy, OutcomeSucceeded, Targets{Worktree: wt, SessionDir: sd})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got != wt {
		t.Errorf("remover got %q", got)
	}
	if exists(sd) {
		t.Error("session should still be removed when the worktree remover fails")
	}
}
