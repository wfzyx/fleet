package git

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Push publishes branch to the origin remote from the worktree at path and
// sets its upstream. It never force-pushes.
func (m *Manager) Push(ctx context.Context, path, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch %q", branch)
	}
	_, err := m.run(ctx, path, "push", "--set-upstream", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

// RemoteRepo returns the "owner/name" GitHub slug of the origin remote of the
// repository at repo.
func (m *Manager) RemoteRepo(ctx context.Context, repo string) (string, error) {
	out, err := m.run(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	return ParseGitHubRemote(strings.TrimSpace(out))
}

// ParseGitHubRemote extracts "owner/name" from an https, ssh:// or scp-style
// git remote URL.
func ParseGitHubRemote(remote string) (string, error) {
	var path string
	switch {
	case strings.Contains(remote, "://"):
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("unrecognised remote %q", remote)
		}
		path = u.Path
	case strings.Contains(remote, ":"): // git@github.com:owner/name.git
		_, p, _ := strings.Cut(remote, ":")
		path = p
	default:
		return "", fmt.Errorf("unrecognised remote %q", remote)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("remote %q is not of the form owner/name", remote)
	}
	return parts[0] + "/" + parts[1], nil
}

// Head returns the commit id HEAD points at in the worktree at path.
func (m *Manager) Head(ctx context.Context, path string) (string, error) {
	out, err := m.run(ctx, path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
