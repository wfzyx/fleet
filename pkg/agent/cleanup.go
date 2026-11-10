package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Outcome is how a role's run ended.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
)

// CleanupPolicy lists, per artifact, the outcomes after which it is removed.
// An artifact not listed for an outcome is kept (for example a failed
// worker's worktree, so a human can inspect it).
type CleanupPolicy struct {
	RemoveWorktree []Outcome `json:"remove_worktree,omitempty"`
	RemoveSession  []Outcome `json:"remove_session,omitempty"`
}

// IsZero reports whether the policy never removes anything.
func (p CleanupPolicy) IsZero() bool {
	return len(p.RemoveWorktree) == 0 && len(p.RemoveSession) == 0
}

// ShouldRemoveWorktree reports whether the worktree goes away after o.
func (p CleanupPolicy) ShouldRemoveWorktree(o Outcome) bool {
	return slices.Contains(p.RemoveWorktree, o)
}

// ShouldRemoveSession reports whether the session directory goes away after o.
func (p CleanupPolicy) ShouldRemoveSession(o Outcome) bool {
	return slices.Contains(p.RemoveSession, o)
}

// Targets are the on-disk artifacts of one finished run.
type Targets struct {
	// Worktree is the task worktree; it must live under WorktreeRoot().
	Worktree string
	// SessionDir is the Pi session directory; it must live under SessionRoot().
	SessionDir string
}

// Cleaner applies cleanup policies. The zero value removes worktrees with
// os.RemoveAll; set RemoveWorktree to integrate `git worktree remove`.
type Cleaner struct {
	// RemoveWorktree removes a worktree directory. Defaults to os.RemoveAll.
	RemoveWorktree func(path string) error
}

// Apply removes the artifacts the policy selects for outcome. Callers must
// record session costs before calling Apply, since sessions may be deleted.
// Paths outside the fleet data directories are refused, never removed.
// All removable artifacts are attempted; errors are joined.
func (c Cleaner) Apply(p CleanupPolicy, outcome Outcome, t Targets) error {
	var errs []error
	if t.Worktree != "" && p.ShouldRemoveWorktree(outcome) {
		errs = append(errs, c.removeWorktree(t.Worktree))
	}
	if t.SessionDir != "" && p.ShouldRemoveSession(outcome) {
		errs = append(errs, removeUnder(SessionRoot, t.SessionDir))
	}
	return errors.Join(errs...)
}

func (c Cleaner) removeWorktree(path string) error {
	root, err := WorktreeRoot()
	if err != nil {
		return err
	}
	if !Within(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return fmt.Errorf("agent: refusing to remove %q: not inside %q", path, root)
	}
	rm := c.RemoveWorktree
	if rm == nil {
		rm = os.RemoveAll
	}
	if err := rm(path); err != nil {
		return fmt.Errorf("agent: remove worktree %q: %w", path, err)
	}
	return nil
}

func removeUnder(rootFn func() (string, error), path string) error {
	root, err := rootFn()
	if err != nil {
		return err
	}
	if !Within(root, path) || filepath.Clean(path) == filepath.Clean(root) {
		return fmt.Errorf("agent: refusing to remove %q: not inside %q", path, root)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("agent: remove %q: %w", path, err)
	}
	return nil
}
