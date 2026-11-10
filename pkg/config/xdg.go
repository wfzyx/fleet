// Package config resolves XDG paths and persists fleet configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// AppName is the directory name used under each XDG base directory.
const AppName = "fleet"

// Paths holds the resolved fleet directories.
type Paths struct {
	ConfigDir  string // $XDG_CONFIG_HOME/fleet
	DataDir    string // $XDG_DATA_HOME/fleet
	RuntimeDir string // $XDG_RUNTIME_DIR/fleet
}

// ErrInvalidID is returned when a space or task ID is unsafe to use as a path segment.
var ErrInvalidID = errors.New("invalid id")

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateID ensures id is safe to use as a single path segment.
func ValidateID(id string) error {
	if !idPattern.MatchString(id) || id == ".." || len(id) > 128 {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// NewPaths resolves paths from the process environment.
func NewPaths() (Paths, error) {
	return ResolvePaths(os.Getenv, os.UserHomeDir, os.Getuid())
}

// ResolvePaths resolves paths using injected environment lookups.
// Per the XDG spec, relative XDG_* values are ignored.
func ResolvePaths(getenv func(string) string, homeDir func() (string, error), uid int) (Paths, error) {
	abs := func(key string) string {
		v := getenv(key)
		if v != "" && filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
		return ""
	}
	home := func() (string, error) {
		h, err := homeDir()
		if err != nil || h == "" || !filepath.IsAbs(h) {
			return "", fmt.Errorf("cannot determine home directory: %w", errors.Join(err, errors.New("home unset or not absolute")))
		}
		return h, nil
	}

	var p Paths
	if base := abs("XDG_CONFIG_HOME"); base != "" {
		p.ConfigDir = filepath.Join(base, AppName)
	} else {
		h, err := home()
		if err != nil {
			return Paths{}, err
		}
		p.ConfigDir = filepath.Join(h, ".config", AppName)
	}
	if base := abs("XDG_DATA_HOME"); base != "" {
		p.DataDir = filepath.Join(base, AppName)
	} else {
		h, err := home()
		if err != nil {
			return Paths{}, err
		}
		p.DataDir = filepath.Join(h, ".local", "share", AppName)
	}
	if base := abs("XDG_RUNTIME_DIR"); base != "" {
		p.RuntimeDir = filepath.Join(base, AppName)
	} else {
		// No runtime dir: fall back to a per-user dir under the temp directory.
		p.RuntimeDir = filepath.Join(os.TempDir(), AppName+"-"+strconv.Itoa(uid))
	}
	return p, nil
}

// ConfigFile is the path of the persisted JSON config.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.json") }

// WorktreesRoot is the directory containing all ephemeral worktrees.
func (p Paths) WorktreesRoot() string { return filepath.Join(p.DataDir, "worktrees") }

// SpaceWorktreesDir is $XDG_DATA_HOME/fleet/worktrees/<space-id>.
func (p Paths) SpaceWorktreesDir(spaceID string) (string, error) {
	if err := ValidateID(spaceID); err != nil {
		return "", fmt.Errorf("space id: %w", err)
	}
	return filepath.Join(p.WorktreesRoot(), spaceID), nil
}

// WorktreePath is $XDG_DATA_HOME/fleet/worktrees/<space-id>/<task-id>.
func (p Paths) WorktreePath(spaceID, taskID string) (string, error) {
	dir, err := p.SpaceWorktreesDir(spaceID)
	if err != nil {
		return "", err
	}
	if err := ValidateID(taskID); err != nil {
		return "", fmt.Errorf("task id: %w", err)
	}
	return filepath.Join(dir, taskID), nil
}

// EnsureDirs creates the config, data, and runtime directories.
// The runtime directory is created with mode 0700.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.ConfigDir, p.DataDir, p.WorktreesRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", p.RuntimeDir, err)
	}
	return nil
}
