package git

import (
	"context"
	"testing"
)

func TestParseGitHubRemote(t *testing.T) {
	good := map[string]string{
		"https://github.com/acme/widgets.git":   "acme/widgets",
		"https://github.com/acme/widgets":       "acme/widgets",
		"https://github.com/acme/widgets/":      "acme/widgets",
		"git@github.com:acme/widgets.git":       "acme/widgets",
		"ssh://git@github.com/acme/widgets.git": "acme/widgets",
		"https://user@ghe.example.com/a/b.git":  "a/b",
	}
	for in, want := range good {
		got, err := ParseGitHubRemote(in)
		if err != nil || got != want {
			t.Errorf("%q: got (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/local/path/repo", "https://github.com/onlyowner", "https://github.com/a/b/c", "git@github.com:x"} {
		if got, err := ParseGitHubRemote(bad); err == nil {
			t.Errorf("%q: expected error, got %q", bad, got)
		}
	}
}

func TestPushAndRemoteRepo(t *testing.T) {
	f := newFixture(t)
	bare := t.TempDir()
	gitIn(t, bare, "init", "--bare", "-q")
	gitIn(t, f.repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	slug, err := f.m.RemoteRepo(f.ctx, f.repo)
	if err != nil || slug != "acme/widgets" {
		t.Fatalf("slug = %q, %v", slug, err)
	}

	gitIn(t, f.repo, "remote", "set-url", "origin", bare)
	wt, err := f.m.Create(f.ctx, CreateOptions{Repo: f.repo, SpaceID: "s", TaskID: "t1", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, wt.Path+"/new.txt", "x\n")
	gitIn(t, wt.Path, "add", "-A")
	gitIn(t, wt.Path, "commit", "-q", "-m", "work")
	if err := f.m.Push(f.ctx, wt.Path, wt.Branch); err != nil {
		t.Fatal(err)
	}
	if out := gitIn(t, bare, "branch", "--list", wt.Branch); out == "" {
		t.Error("branch not pushed")
	}
	if err := f.m.Push(context.Background(), wt.Path, "-bad"); err == nil {
		t.Error("expected error for option-like branch")
	}
}
