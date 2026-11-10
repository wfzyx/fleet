package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AppName is the directory name used below the XDG base directories.
const AppName = "fleet"

// ErrInvalidName is returned for space or task names that are not safe to use
// as a single path element.
var ErrInvalidName = errors.New("agent: invalid name")

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateName reports whether name is safe to embed in a path (no separators,
// no leading dot, no "..", bounded length).
func ValidateName(kind, name string) error {
	if !nameRE.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w: %s %q (want [A-Za-z0-9][A-Za-z0-9._-]*, no \"..\")", ErrInvalidName, kind, name)
	}
	return nil
}

// DataHome returns $XDG_DATA_HOME, defaulting to ~/.local/share. Relative
// values are ignored as required by the XDG specification.
func DataHome() (string, error) {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" && filepath.IsAbs(v) {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("agent: resolve data home: %w", err)
	}
	return filepath.Join(home, ".local", "share"), nil
}

// DataDir returns $XDG_DATA_HOME/fleet.
func DataDir() (string, error) {
	dh, err := DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dh, AppName), nil
}

// WorktreeRoot returns $XDG_DATA_HOME/fleet/worktrees.
func WorktreeRoot() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "worktrees"), nil
}

// WorktreeDir returns $XDG_DATA_HOME/fleet/worktrees/<space>/<task>.
func WorktreeDir(space, task string) (string, error) {
	if err := validatePair(space, task); err != nil {
		return "", err
	}
	root, err := WorktreeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, space, task), nil
}

// SessionRoot returns $XDG_DATA_HOME/fleet/sessions.
func SessionRoot() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "sessions"), nil
}

// SessionDir returns the Pi session directory for a role.
//
//	Lead:     sessions/<space>/lead
//	Worker:   sessions/<space>/tasks/<task>/worker
//	Reviewer: sessions/<space>/tasks/<task>/reviewer
//
// task is ignored for the lead.
func SessionDir(role Role, space, task string) (string, error) {
	if err := ValidateName("space", space); err != nil {
		return "", err
	}
	root, err := SessionRoot()
	if err != nil {
		return "", err
	}
	switch role {
	case RoleLead:
		return filepath.Join(root, space, "lead"), nil
	case RoleWorker, RoleReviewer:
		if err := ValidateName("task", task); err != nil {
			return "", err
		}
		return filepath.Join(root, space, "tasks", task, string(role)), nil
	default:
		return "", fmt.Errorf("agent: unknown role %q", role)
	}
}

// SkillsDir returns $XDG_DATA_HOME/fleet/skills.
func SkillsDir() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "skills"), nil
}

func validatePair(space, task string) error {
	if err := ValidateName("space", space); err != nil {
		return err
	}
	return ValidateName("task", task)
}

// Within reports whether path is lexically inside (or equal to) root, after
// cleaning both. It does not touch the filesystem.
func Within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
