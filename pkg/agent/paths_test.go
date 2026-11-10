package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateName(t *testing.T) {
	good := []string{"a", "my-space", "task_1", "ot-1903.hover", "A1"}
	bad := []string{"", ".", "..", ".hidden", "a/b", "a\\b", "a b", "a..b", "-x", "../x", "x\n", string(make([]byte, 200))}
	for _, n := range good {
		if err := ValidateName("task", n); err != nil {
			t.Errorf("%q rejected: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateName("task", n); !errors.Is(err, ErrInvalidName) {
			t.Errorf("%q accepted (err=%v)", n, err)
		}
	}
}

func TestDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/data")
	if got, _ := DataHome(); got != "/custom/data" {
		t.Errorf("got %q", got)
	}
	t.Setenv("XDG_DATA_HOME", "relative/path") // must be ignored per XDG spec
	t.Setenv("HOME", "/home/u")
	got, err := DataHome()
	if err != nil || got != "/home/u/.local/share" {
		t.Errorf("got %q, %v", got, err)
	}
	t.Setenv("XDG_DATA_HOME", "")
	if got, _ := DataHome(); got != "/home/u/.local/share" {
		t.Errorf("got %q", got)
	}
}

func TestPaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/x")
	wt, err := WorktreeDir("sp", "t1")
	if err != nil || wt != "/x/fleet/worktrees/sp/t1" {
		t.Errorf("worktree = %q, %v", wt, err)
	}
	cases := []struct {
		role Role
		task string
		want string
	}{
		{RoleLead, "", "/x/fleet/sessions/sp/lead"},
		{RoleLead, "ignored", "/x/fleet/sessions/sp/lead"},
		{RoleWorker, "t1", "/x/fleet/sessions/sp/tasks/t1/worker"},
		{RoleReviewer, "t1", "/x/fleet/sessions/sp/tasks/t1/reviewer"},
	}
	for _, c := range cases {
		got, err := SessionDir(c.role, "sp", c.task)
		if err != nil || got != c.want {
			t.Errorf("SessionDir(%s,%q) = %q, %v; want %q", c.role, c.task, got, err, c.want)
		}
	}
	if _, err := SessionDir(RoleWorker, "sp", ""); err == nil {
		t.Error("worker session without task should fail")
	}
	if _, err := SessionDir("nope", "sp", "t"); err == nil {
		t.Error("unknown role should fail")
	}
	if _, err := WorktreeDir("../evil", "t"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("traversal accepted: %v", err)
	}
	if _, err := WorktreeDir("sp", "a/b"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("separator accepted: %v", err)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		root, path string
		want       bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c/d", true},
		{"/a/b", "/a/b/../c", false},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b/", "/a/b/c", true},
	}
	for _, c := range cases {
		if got := Within(c.root, c.path); got != c.want {
			t.Errorf("Within(%q,%q) = %v, want %v", c.root, c.path, got, c.want)
		}
	}
}

func TestSkillsDirUnderData(t *testing.T) {
	d := t.TempDir()
	t.Setenv("XDG_DATA_HOME", d)
	got, _ := SkillsDir()
	if got != filepath.Join(d, "fleet", "skills") {
		t.Errorf("got %q", got)
	}
	if _, err := os.Stat(got); err == nil {
		t.Error("SkillsDir must not create the directory")
	}
}
