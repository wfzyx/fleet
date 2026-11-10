// Package git manages ephemeral git worktrees for fleet tasks.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"fleet/pkg/config"
)

// ErrNoBaseBranch is returned when no base branch candidate exists.
var ErrNoBaseBranch = errors.New("no base branch found")

// DefaultBaseCandidates is the lookup order when no base is given.
var DefaultBaseCandidates = []string{"origin/main", "origin/master", "main", "master"}

// Manager creates and removes worktrees under the XDG data directory.
type Manager struct {
	Paths config.Paths
	// Git is the git binary; defaults to "git".
	Git string
}

// NewManager returns a Manager using the given paths.
func NewManager(p config.Paths) *Manager { return &Manager{Paths: p, Git: "git"} }

// Worktree describes a created worktree.
type Worktree struct {
	Repo    string // main checkout the worktree belongs to
	Path    string // $XDG_DATA_HOME/fleet/worktrees/<space>/<task>
	Branch  string
	Base    string // resolved base ref, e.g. origin/main
	SpaceID string
	TaskID  string
}

// CreateOptions configures Create.
type CreateOptions struct {
	Repo    string // path to the user's checkout (never modified in its working tree)
	SpaceID string
	TaskID  string
	Branch  string // new branch name; defaults to fleet/<task-id>
	Base    string // base ref; empty means first of DefaultBaseCandidates
}

func (m *Manager) bin() string {
	if m.Git == "" {
		return "git"
	}
	return m.Git
}

// run executes git in dir and returns stdout. Stderr is included in errors.
func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, m.bin(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ResolveBase returns the first existing ref among candidates (or
// DefaultBaseCandidates when empty) in repo.
func (m *Manager) ResolveBase(ctx context.Context, repo string, candidates ...string) (string, error) {
	if len(candidates) == 0 {
		candidates = DefaultBaseCandidates
	}
	for _, c := range candidates {
		if c == "" || strings.HasPrefix(c, "-") {
			continue
		}
		if _, err := m.run(ctx, repo, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w among %v", ErrNoBaseBranch, candidates)
}

// Create adds a new worktree and branch at the XDG worktree path.
func (m *Manager) Create(ctx context.Context, o CreateOptions) (*Worktree, error) {
	path, err := m.Paths.WorktreePath(o.SpaceID, o.TaskID)
	if err != nil {
		return nil, err
	}
	if o.Repo == "" {
		return nil, errors.New("repo is required")
	}
	repo, err := filepath.Abs(o.Repo)
	if err != nil {
		return nil, err
	}
	branch := o.Branch
	if branch == "" {
		branch = "fleet/" + o.TaskID
	}
	if strings.HasPrefix(branch, "-") {
		return nil, fmt.Errorf("invalid branch name %q", branch)
	}
	if _, err := m.run(ctx, repo, "check-ref-format", "--branch", branch); err != nil {
		return nil, fmt.Errorf("invalid branch name %q: %w", branch, err)
	}
	var base string
	if o.Base != "" {
		base, err = m.ResolveBase(ctx, repo, o.Base)
	} else {
		base, err = m.ResolveBase(ctx, repo)
	}
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("worktree path already exists: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent: %w", err)
	}
	// --no-track: the task branch should not track the base.
	if _, err := m.run(ctx, repo, "worktree", "add", "--no-track", "-b", branch, path, base); err != nil {
		return nil, err
	}
	return &Worktree{Repo: repo, Path: path, Branch: branch, Base: base, SpaceID: o.SpaceID, TaskID: o.TaskID}, nil
}

// Status reports the porcelain status of the worktree.
type Status struct {
	Clean     bool
	Modified  []string // tracked files with staged or unstaged changes
	Untracked []string
}

// Status returns the clean/dirty state of the worktree at path.
func (m *Manager) Status(ctx context.Context, path string) (Status, error) {
	out, err := m.run(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Status{}, err
	}
	var s Status
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		code, name := e[:2], e[3:]
		if code[0] == 'R' || code[0] == 'C' {
			i++ // skip the original-path entry
		}
		if code == "??" {
			s.Untracked = append(s.Untracked, name)
		} else {
			s.Modified = append(s.Modified, name)
		}
	}
	s.Clean = len(s.Modified) == 0 && len(s.Untracked) == 0
	return s, nil
}

// IsClean reports whether the worktree has no changes and no untracked files.
func (m *Manager) IsClean(ctx context.Context, path string) (bool, error) {
	s, err := m.Status(ctx, path)
	return s.Clean, err
}

// CurrentBranch returns the checked-out branch name of the worktree.
func (m *Manager) CurrentBranch(ctx context.Context, path string) (string, error) {
	out, err := m.run(ctx, path, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Diff returns a unified diff of everything the worktree changed relative to
// the merge-base with base: commits, staged, unstaged and untracked files.
// Untracked files are registered with intent-to-add in the worktree's index
// (the worktree is ephemeral; the user's checkout is untouched).
func (m *Manager) Diff(ctx context.Context, path, base string) (string, error) {
	if base == "" || strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("invalid base %q", base)
	}
	mb, err := m.run(ctx, path, "merge-base", base, "HEAD")
	if err != nil {
		return "", err
	}
	mergeBase := strings.TrimSpace(mb)
	if _, err := m.run(ctx, path, "add", "--intent-to-add", "--all"); err != nil {
		return "", err
	}
	return m.run(ctx, path, "diff", "--no-color", "--no-ext-diff", "--unified=3", mergeBase, "--")
}

// Remove deletes the worktree at path and, when deleteBranch is set, its
// branch. The path must live under the managed worktrees root. Removing a
// worktree that no longer exists is not an error.
func (m *Manager) Remove(ctx context.Context, w *Worktree, deleteBranch bool) error {
	if w == nil {
		return errors.New("nil worktree")
	}
	want, err := m.Paths.WorktreePath(w.SpaceID, w.TaskID)
	if err != nil {
		return err
	}
	if filepath.Clean(w.Path) != want {
		return fmt.Errorf("refusing to remove %s: not the managed path %s", w.Path, want)
	}
	_, statErr := os.Lstat(want)
	if statErr == nil {
		if _, err := m.run(ctx, w.Repo, "worktree", "remove", "--force", want); err != nil {
			// Fall back for broken worktrees (e.g. missing .git file).
			if rerr := os.RemoveAll(want); rerr != nil {
				return errors.Join(err, rerr)
			}
		}
	}
	if _, err := m.run(ctx, w.Repo, "worktree", "prune"); err != nil {
		return err
	}
	if deleteBranch && w.Branch != "" {
		if _, err := m.run(ctx, w.Repo, "branch", "-D", w.Branch); err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}
	}
	// Remove the space directory if now empty; ignore failure when non-empty.
	_ = os.Remove(filepath.Dir(want))
	return nil
}
