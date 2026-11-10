package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
func home(h string) func() (string, error)        { return func() (string, error) { return h, nil } }

func TestResolvePathsXDG(t *testing.T) {
	p, err := ResolvePaths(env(map[string]string{
		"XDG_CONFIG_HOME": "/c", "XDG_DATA_HOME": "/d", "XDG_RUNTIME_DIR": "/r",
	}), home("/home/u"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/c/fleet" || p.DataDir != "/d/fleet" || p.RuntimeDir != "/r/fleet" {
		t.Fatalf("got %+v", p)
	}
	if p.ConfigFile() != "/c/fleet/config.json" {
		t.Fatal(p.ConfigFile())
	}
}

func TestResolvePathsFallbacks(t *testing.T) {
	p, err := ResolvePaths(env(nil), home("/home/u"), 42)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/home/u/.config/fleet" || p.DataDir != "/home/u/.local/share/fleet" {
		t.Fatalf("got %+v", p)
	}
	if !strings.HasSuffix(p.RuntimeDir, "fleet-42") {
		t.Fatal(p.RuntimeDir)
	}
}

func TestResolvePathsIgnoresRelative(t *testing.T) {
	p, err := ResolvePaths(env(map[string]string{"XDG_CONFIG_HOME": "rel", "XDG_DATA_HOME": "./x"}), home("/h"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != "/h/.config/fleet" || p.DataDir != "/h/.local/share/fleet" {
		t.Fatalf("got %+v", p)
	}
}

func TestResolvePathsNoHome(t *testing.T) {
	if _, err := ResolvePaths(env(nil), func() (string, error) { return "", errors.New("boom") }, 1); err == nil {
		t.Fatal("expected error")
	}
	// Absolute XDG vars need no home.
	if _, err := ResolvePaths(env(map[string]string{"XDG_CONFIG_HOME": "/c", "XDG_DATA_HOME": "/d"}),
		func() (string, error) { return "", errors.New("boom") }, 1); err != nil {
		t.Fatal(err)
	}
}

func TestNewPathsFromEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	t.Setenv("XDG_DATA_HOME", "/tmp/data")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/run")
	p, err := NewPaths()
	if err != nil || p.DataDir != "/tmp/data/fleet" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestWorktreePath(t *testing.T) {
	p := Paths{DataDir: "/d/fleet"}
	got, err := p.WorktreePath("space1", "task-2")
	if err != nil || got != "/d/fleet/worktrees/space1/task-2" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestWorktreePathRejectsBadIDs(t *testing.T) {
	p := Paths{DataDir: "/d/fleet"}
	for _, id := range []string{"", "..", ".", "a/b", "../x", "-rf", ".hidden", "a b", "a\x00b", strings.Repeat("a", 200)} {
		if _, err := p.WorktreePath(id, "t"); !errors.Is(err, ErrInvalidID) {
			t.Errorf("space %q: err=%v", id, err)
		}
		if _, err := p.WorktreePath("s", id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("task %q: err=%v", id, err)
		}
	}
}

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	p := Paths{
		ConfigDir:  filepath.Join(root, "c", "fleet"),
		DataDir:    filepath.Join(root, "d", "fleet"),
		RuntimeDir: filepath.Join(root, "r", "fleet"),
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.ConfigDir, p.DataDir, p.WorktreesRoot(), p.RuntimeDir} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("%s missing: %v", d, err)
		}
	}
	st, _ := os.Stat(p.RuntimeDir)
	if st.Mode().Perm() != 0o700 {
		t.Errorf("runtime perm %v", st.Mode().Perm())
	}
}
