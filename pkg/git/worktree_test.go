package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fleet/pkg/config"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	m    *Manager
	repo string
	ctx  context.Context
}

// newFixture builds a repo with one commit on branch main, and a manager whose
// data dir is a temp dir.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireGit(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.test")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.test")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "init", "-q", "-b", "main", repo)
	write(t, filepath.Join(repo, "a.txt"), "one\ntwo\n")
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	paths := config.Paths{
		ConfigDir: filepath.Join(root, "cfg"),
		DataDir:   filepath.Join(root, "data", "fleet"),
	}
	return &fixture{m: NewManager(paths), repo: repo, ctx: context.Background()}
}

func (f *fixture) create(t *testing.T) *Worktree {
	t.Helper()
	w, err := f.m.Create(f.ctx, CreateOptions{Repo: f.repo, SpaceID: "space", TaskID: "task1"})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestCreateAtXDGPathAndRepoStaysClean(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	want := filepath.Join(f.m.Paths.DataDir, "worktrees", "space", "task1")
	if w.Path != want {
		t.Fatalf("path %s want %s", w.Path, want)
	}
	if w.Branch != "fleet/task1" || w.Base != "main" {
		t.Fatalf("%+v", w)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "a.txt")); err != nil {
		t.Fatal(err)
	}
	br, err := f.m.CurrentBranch(f.ctx, w.Path)
	if err != nil || br != "fleet/task1" {
		t.Fatalf("%q %v", br, err)
	}
	// User checkout: clean and still on main, and no files created inside it.
	if clean, err := f.m.IsClean(f.ctx, f.repo); err != nil || !clean {
		t.Fatalf("user repo dirty: %v", err)
	}
	if b, _ := f.m.CurrentBranch(f.ctx, f.repo); b != "main" {
		t.Fatalf("user repo branch %q", b)
	}
	ents, _ := os.ReadDir(f.repo)
	for _, e := range ents {
		if e.Name() != ".git" && e.Name() != "a.txt" {
			t.Errorf("unexpected file in user checkout: %s", e.Name())
		}
	}
}

func TestCreateErrors(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	cases := map[string]CreateOptions{
		"duplicate":    {Repo: f.repo, SpaceID: "space", TaskID: "task1"},
		"bad space":    {Repo: f.repo, SpaceID: "../x", TaskID: "t"},
		"bad task":     {Repo: f.repo, SpaceID: "space", TaskID: "a/b"},
		"no repo":      {SpaceID: "space", TaskID: "t2"},
		"not a repo":   {Repo: t.TempDir(), SpaceID: "space", TaskID: "t3"},
		"bad branch":   {Repo: f.repo, SpaceID: "space", TaskID: "t4", Branch: "a..b"},
		"dash branch":  {Repo: f.repo, SpaceID: "space", TaskID: "t5", Branch: "-x"},
		"missing base": {Repo: f.repo, SpaceID: "space", TaskID: "t6", Base: "origin/nope"},
		"branch taken": {Repo: f.repo, SpaceID: "space", TaskID: "t7", Branch: w.Branch},
	}
	for name, o := range cases {
		if _, err := f.m.Create(f.ctx, o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestResolveBase(t *testing.T) {
	f := newFixture(t)
	got, err := f.m.ResolveBase(f.ctx, f.repo)
	if err != nil || got != "main" {
		t.Fatalf("%q %v", got, err)
	}
	// Add a remote-tracking ref origin/main; it takes precedence.
	gitIn(t, f.repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	got, err = f.m.ResolveBase(f.ctx, f.repo)
	if err != nil || got != "origin/main" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := f.m.ResolveBase(f.ctx, f.repo, "nope", "main"); got != "main" {
		t.Fatalf("%q", got)
	}
	if _, err := f.m.ResolveBase(f.ctx, f.repo, "nope", "-x", ""); err == nil || !strings.Contains(err.Error(), "no base branch") {
		t.Fatalf("%v", err)
	}
}

func TestResolveBasePrefersOriginAndWorktreeBranchesFromIt(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.repo, "b.txt"), "b\n")
	gitIn(t, f.repo, "add", ".")
	gitIn(t, f.repo, "commit", "-q", "-m", "second")
	gitIn(t, f.repo, "update-ref", "refs/remotes/origin/main", "HEAD~1")
	w := f.create(t)
	if w.Base != "origin/main" {
		t.Fatalf("base %s", w.Base)
	}
	if _, err := os.Stat(filepath.Join(w.Path, "b.txt")); !os.IsNotExist(err) {
		t.Fatal("worktree should branch from origin/main (without b.txt)")
	}
}

func TestStatusCleanDirty(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	if clean, err := f.m.IsClean(f.ctx, w.Path); err != nil || !clean {
		t.Fatalf("expected clean: %v", err)
	}
	write(t, filepath.Join(w.Path, "a.txt"), "changed\n")
	write(t, filepath.Join(w.Path, "dir", "new file.txt"), "n\n")
	s, err := f.m.Status(f.ctx, w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Clean || len(s.Modified) != 1 || s.Modified[0] != "a.txt" ||
		len(s.Untracked) != 1 || s.Untracked[0] != "dir/new file.txt" {
		t.Fatalf("%+v", s)
	}
}

func TestStatusRename(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	gitIn(t, w.Path, "mv", "a.txt", "renamed.txt")
	s, err := f.m.Status(f.ctx, w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Clean || len(s.Modified) != 1 || s.Modified[0] != "renamed.txt" || len(s.Untracked) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestDiff(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	d, err := f.m.Diff(f.ctx, w.Path, w.Base)
	if err != nil || d != "" {
		t.Fatalf("expected empty diff: %q %v", d, err)
	}
	// Committed change, unstaged change and untracked file all appear.
	write(t, filepath.Join(w.Path, "c.txt"), "committed\n")
	gitIn(t, w.Path, "add", ".")
	gitIn(t, w.Path, "commit", "-q", "-m", "c")
	write(t, filepath.Join(w.Path, "a.txt"), "one\nTWO\n")
	write(t, filepath.Join(w.Path, "u.txt"), "untracked\n")
	d, err = f.m.Diff(f.ctx, w.Path, w.Base)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff --git a/c.txt b/c.txt", "+committed", "-two", "+TWO", "diff --git a/u.txt b/u.txt", "+untracked"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
}

func TestDiffIgnoresBaseAdvancing(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	write(t, filepath.Join(f.repo, "main-only.txt"), "m\n")
	gitIn(t, f.repo, "add", ".")
	gitIn(t, f.repo, "commit", "-q", "-m", "main moves")
	write(t, filepath.Join(w.Path, "w.txt"), "w\n")
	d, err := f.m.Diff(f.ctx, w.Path, "main")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d, "main-only") || !strings.Contains(d, "w.txt") {
		t.Fatalf("diff should be against merge-base:\n%s", d)
	}
}

func TestDiffErrors(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	for _, base := range []string{"", "--output=/tmp/x", "does-not-exist"} {
		if _, err := f.m.Diff(f.ctx, w.Path, base); err == nil {
			t.Errorf("base %q: expected error", base)
		}
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	write(t, filepath.Join(w.Path, "dirty.txt"), "x\n") // dirty worktrees are still removed
	if err := f.m.Remove(f.ctx, w, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(w.Path)); !os.IsNotExist(err) {
		t.Fatal("empty space dir not removed")
	}
	if out := gitIn(t, f.repo, "branch", "--list", w.Branch); strings.TrimSpace(out) != "" {
		t.Fatalf("branch not deleted: %q", out)
	}
	if out := gitIn(t, f.repo, "worktree", "list", "--porcelain"); strings.Contains(out, "task1") {
		t.Fatalf("worktree still registered:\n%s", out)
	}
	// Idempotent.
	if err := f.m.Remove(f.ctx, w, true); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

func TestRemoveKeepsBranchAndSiblings(t *testing.T) {
	f := newFixture(t)
	w1 := f.create(t)
	w2, err := f.m.Create(f.ctx, CreateOptions{Repo: f.repo, SpaceID: "space", TaskID: "task2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Remove(f.ctx, w1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w2.Path); err != nil {
		t.Fatal("sibling removed")
	}
	if out := gitIn(t, f.repo, "branch", "--list", w1.Branch); strings.TrimSpace(out) == "" {
		t.Fatal("branch should be kept")
	}
}

func TestRemoveBrokenWorktree(t *testing.T) {
	f := newFixture(t)
	w := f.create(t)
	os.Remove(filepath.Join(w.Path, ".git"))
	if err := f.m.Remove(f.ctx, w, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatal("still exists")
	}
}

func TestRemoveRefusesUnmanagedPath(t *testing.T) {
	f := newFixture(t)
	victim := t.TempDir()
	write(t, filepath.Join(victim, "keep"), "x")
	err := f.m.Remove(f.ctx, &Worktree{Repo: f.repo, Path: victim, SpaceID: "space", TaskID: "task1"}, false)
	if err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatal("unmanaged path was touched")
	}
	if err := f.m.Remove(f.ctx, nil, false); err == nil {
		t.Fatal("nil worktree accepted")
	}
	if err := f.m.Remove(f.ctx, &Worktree{Repo: f.repo, Path: "/x", SpaceID: "..", TaskID: "t"}, false); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestContextCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.m.Create(ctx, CreateOptions{Repo: f.repo, SpaceID: "s", TaskID: "t"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(f.m.Paths.DataDir, "worktrees", "s", "t")); !os.IsNotExist(err) {
		t.Fatal("worktree path created despite cancel")
	}
}
