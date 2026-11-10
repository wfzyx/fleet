package pi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"fleet/pkg/agent"
)

// fakePi is a stand-in for the pi binary. It records its argv (NUL separated),
// cwd, environment and stdin under $FAKE_OUT, copies $FAKE_SESSION into the
// --session-dir, and prints $FAKE_STDOUT.
const fakePi = `#!/bin/sh
out="$FAKE_OUT"
: > "$out/args"
for a in "$@"; do printf '%s\0' "$a" >> "$out/args"; done
pwd > "$out/cwd"
env > "$out/env"
case "$FAKE_MODE" in
rpc)
  while IFS= read -r line; do printf '{"type":"echo","got":%s}\n' "$line"; done
  exit 0 ;;
sleep)
  echo started > "$out/started"
  sleep 30
  exit 0 ;;
esac
cat > "$out/stdin"
sd=""; prev=""
for a in "$@"; do [ "$prev" = "--session-dir" ] && sd="$a"; prev="$a"; done
if [ -n "$sd" ] && [ -n "$FAKE_SESSION" ]; then cp "$FAKE_SESSION" "$sd/s.jsonl"; fi
[ -n "$FAKE_STDERR" ] && echo "$FAKE_STDERR" >&2
printf '%s' "$FAKE_STDOUT"
exit "${FAKE_EXIT:-0}"
`

type fake struct {
	bin string
	out string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	if err := os.WriteFile(bin, []byte(fakePi), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return &fake{bin: bin, out: out}
}

// env returns a base environment containing the fake's control variables.
func (f *fake) env(kv ...string) []string {
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.out, "FAKE_OUT=" + f.out}
	return append(base, kv...)
}

func (f *fake) common(kv ...string) Common {
	return Common{Binary: f.bin, Env: f.env(kv...)}
}

func (f *fake) args(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.out, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func (f *fake) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.out, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hasFlag(args []string, flag string) bool { return slices.Contains(args, flag) }

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("flag %s missing in %q", flag, args)
	}
	return args[i+1]
}

// setupWorktree creates $XDG_DATA_HOME/fleet/worktrees/<space>/<task>.
func setupWorktree(t *testing.T, space, task, agentsMD string) string {
	t.Helper()
	wt, err := agent.WorktreeDir(space, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if agentsMD != "" {
		if err := os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte(agentsMD), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return wt
}

func xdg(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_DATA_HOME", d)
	return d
}

func sessionFile(t *testing.T, cost float64) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), "sess.jsonl"),
		`{"type":"session","id":"x","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+
			assistantLine("a", "2026-01-01T00:01:00Z", "anthropic", "claude-sonnet-4-5", usageJSON(10, 10, 0, 0, cost))+"\n")
}

// ------------------------------------------------------------------ Worker

func TestBuildWorkerCommand(t *testing.T) {
	d := xdg(t)
	wt := setupWorktree(t, "web", "t1", "Always run `go test`.\n")
	f := newFake(t)
	c := f.common(
		"TOPH_MEMORY_DIR=/secret/memory", // must be scrubbed
		"PI_SESSION_FILE=/parent/session.jsonl",
		"FLEET_HOME=/fleet/home", // must be kept: workers call the fleet CLI
		"KEEP_ME=1",
	)
	c.Extensions = []string{"/ext/claude-bridge.ts"}

	cmd, targets, err := BuildWorkerCommand(WorkerOptions{
		Common: c, Space: "web", Task: "t1", Title: "Fix login", Brief: "Fix the login bug.",
		Branch: "fix/login", Acceptance: []string{"login works"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != f.bin || cmd.Dir != wt {
		t.Errorf("path=%q dir=%q", cmd.Path, cmd.Dir)
	}
	wantSessions := filepath.Join(d, "fleet", "sessions", "web", "tasks", "t1", "worker")
	if targets.Worktree != wt || targets.SessionDir != wantSessions {
		t.Errorf("targets = %+v", targets)
	}
	if fi, err := os.Stat(wantSessions); err != nil || !fi.IsDir() {
		t.Errorf("session dir not created: %v", err)
	}

	a := cmd.Args
	if flagValue(t, a, "--model") != "sonnet" || flagValue(t, a, "--thinking") != "medium" {
		t.Errorf("model/thinking: %q", a)
	}
	if flagValue(t, a, "--session-dir") != wantSessions {
		t.Errorf("session-dir = %q", flagValue(t, a, "--session-dir"))
	}
	// Isolation: no ambient memory, skills, templates, context files, MCP.
	for _, flag := range []string{"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-mcp", "--print"} {
		if !hasFlag(a, flag) {
			t.Errorf("missing %s in %q", flag, a)
		}
	}
	if flagValue(t, a, "-e") != "/ext/claude-bridge.ts" {
		t.Errorf("provider extension not forwarded: %q", a)
	}
	if hasFlag(a, "--skill") || hasFlag(a, "--session-id") || hasFlag(a, "--continue") || hasFlag(a, "--resume") {
		t.Errorf("worker must not load skills or resume sessions: %q", a)
	}
	sys := flagValue(t, a, "--append-system-prompt")
	if !strings.Contains(sys, agent.WorkerSystemPrompt) || !strings.Contains(sys, "Always run `go test`.") {
		t.Errorf("system prompt lacks role prompt / AGENTS.md:\n%s", sys)
	}
	// Task prompt is the last arg after "--".
	if a[len(a)-2] != "--" || !strings.Contains(a[len(a)-1], "Fix the login bug.") || !strings.Contains(a[len(a)-1], "1. login works") {
		t.Errorf("prompt arg wrong: %q", a[len(a)-2:])
	}

	env := strings.Join(cmd.Env, "\n")
	for _, bad := range []string{"TOPH_MEMORY_DIR", "PI_SESSION_FILE"} {
		if strings.Contains(env, bad) {
			t.Errorf("env leaks %s", bad)
		}
	}
	for _, want := range []string{"KEEP_ME=1", "FLEET_HOME=/fleet/home", "FLEET_ROLE=worker", "FLEET_SPACE=web", "FLEET_TASK=t1", "FLEET_WORKTREE=" + wt} {
		if !slices.Contains(cmd.Env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

func TestBuildWorkerCommandNoAgentsMD(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	cmd, _, err := BuildWorkerCommand(WorkerOptions{Common: newFake(t).common(), Space: "web", Task: "t1", Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got := flagValue(t, cmd.Args, "--append-system-prompt"); got != agent.WorkerSystemPrompt {
		t.Errorf("system prompt = %q", got)
	}
}

func TestBuildWorkerCommandAgentsMDTruncated(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", strings.Repeat("x", maxContextFileBytes*2))
	cmd, _, err := BuildWorkerCommand(WorkerOptions{Common: newFake(t).common(), Space: "web", Task: "t1", Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	sys := flagValue(t, cmd.Args, "--append-system-prompt")
	if !strings.Contains(sys, "[AGENTS.md truncated]") || len(sys) > maxContextFileBytes+1024 {
		t.Errorf("AGENTS.md not capped: %d bytes", len(sys))
	}
}

func TestBuildWorkerCommandErrors(t *testing.T) {
	d := xdg(t)
	f := newFake(t)
	setupWorktree(t, "web", "t1", "")
	ok := WorkerOptions{Common: f.common(), Space: "web", Task: "t1", Brief: "b"}

	t.Run("missing worktree", func(t *testing.T) {
		o := ok
		o.Task = "nope"
		if _, _, err := BuildWorkerCommand(o); !errors.Is(err, ErrWorktreeMissing) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("outside root", func(t *testing.T) {
		o := ok
		o.Worktree = t.TempDir()
		if _, _, err := BuildWorkerCommand(o); !errors.Is(err, ErrOutsideWorktreeRoot) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("traversal via override", func(t *testing.T) {
		o := ok
		o.Worktree = filepath.Join(d, "fleet", "worktrees", "..", "..", "elsewhere")
		if _, _, err := BuildWorkerCommand(o); !errors.Is(err, ErrOutsideWorktreeRoot) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bad names", func(t *testing.T) {
		o := ok
		o.Task = "../x"
		if _, _, err := BuildWorkerCommand(o); !errors.Is(err, agent.ErrInvalidName) {
			t.Errorf("err = %v", err)
		}
		o = ok
		o.Space = "a/b"
		o.Worktree = filepath.Join(d, "fleet", "worktrees", "web", "t1")
		if _, _, err := BuildWorkerCommand(o); !errors.Is(err, agent.ErrInvalidName) {
			t.Errorf("override with bad space: %v", err)
		}
	})
	t.Run("empty brief", func(t *testing.T) {
		o := ok
		o.Brief = " "
		if _, _, err := BuildWorkerCommand(o); !errors.Is(err, agent.ErrInvalidPrompt) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("config for wrong role", func(t *testing.T) {
		o := ok
		o.Config = agent.DefaultConfig(agent.RoleReviewer)
		if _, _, err := BuildWorkerCommand(o); err == nil {
			t.Error("expected role mismatch")
		}
	})
	t.Run("worker may not inherit user context", func(t *testing.T) {
		o := ok
		o.Config = agent.DefaultConfig(agent.RoleWorker)
		o.Config.InheritUserContext = true
		if _, _, err := BuildWorkerCommand(o); err == nil {
			t.Error("inherit accepted: memories would leak into workers")
		}
	})
}

func TestBuildWorkerCommandModelOverride(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	cfg := agent.DefaultConfig(agent.RoleWorker)
	cfg.Model, cfg.Thinking = "prov/custom", ""
	c := newFake(t).common()
	c.Config = cfg
	cmd, _, err := BuildWorkerCommand(WorkerOptions{Common: c, Space: "web", Task: "t1", Brief: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if flagValue(t, cmd.Args, "--model") != "prov/custom" || hasFlag(cmd.Args, "--thinking") {
		t.Errorf("args = %q", cmd.Args)
	}
}

func TestRunWorker(t *testing.T) {
	xdg(t)
	wt := setupWorktree(t, "web", "t1", "repo rules")
	f := newFake(t)
	tr := NewTracker(Budgets{Task: 5})
	c := f.common("FAKE_STDOUT=all done", "FAKE_SESSION="+sessionFile(t, 0.75), "FAKE_STDERR=warn")
	c.Tracker = tr

	res, err := RunWorker(context.Background(), WorkerOptions{Common: c, Space: "web", Task: "t1", Brief: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "all done" || res.Outcome != agent.OutcomeSucceeded || res.ExitCode != 0 || !strings.Contains(res.Stderr, "warn") {
		t.Errorf("result = %+v", res)
	}
	if res.Sessions == nil || !approx(res.Sessions.Total.Cost.Total, 0.75) || res.Sessions.PrimaryModel() != "anthropic/claude-sonnet-4-5" {
		t.Errorf("sessions = %+v", res.Sessions)
	}
	if got := tr.TaskUsage("web", "t1").Cost.Total; !approx(got, 0.75) {
		t.Errorf("tracker cost = %v", got)
	}
	// The process ran in the worktree.
	if got := strings.TrimSpace(f.read(t, "cwd")); !sameDir(got, wt) {
		t.Errorf("cwd = %q, want %q", got, wt)
	}

	// Succeeded worker: cleanup removes worktree and session, but the spend
	// recorded in the tracker survives.
	if err := res.Cleanup(agent.Cleaner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("worktree not cleaned")
	}
	if _, err := os.Stat(res.Targets.SessionDir); !os.IsNotExist(err) {
		t.Error("session dir not cleaned")
	}
	if got := tr.TaskUsage("web", "t1").Cost.Total; !approx(got, 0.75) {
		t.Errorf("cost lost after cleanup: %v", got)
	}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func TestRunWorkerFailureKeepsArtifacts(t *testing.T) {
	xdg(t)
	wt := setupWorktree(t, "web", "t1", "")
	f := newFake(t)
	tr := NewTracker(Budgets{})
	c := f.common("FAKE_EXIT=3", "FAKE_STDERR=model exploded", "FAKE_SESSION="+sessionFile(t, 0.2))
	c.Tracker = tr

	res, err := RunWorker(context.Background(), WorkerOptions{Common: c, Space: "web", Task: "t1", Brief: "b"})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || !strings.Contains(ee.Error(), "model exploded") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Outcome != agent.OutcomeFailed || res.ExitCode != 3 {
		t.Fatalf("res = %+v", res)
	}
	// Spend of a failed run is still accounted.
	if got := tr.TaskUsage("web", "t1").Cost.Total; !approx(got, 0.2) {
		t.Errorf("failed run spend = %v", got)
	}
	if err := res.Cleanup(agent.Cleaner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Error("failed worker's worktree must be kept")
	}
}

func TestRunWorkerBudgetBlocksLaunch(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	f := newFake(t)
	tr := NewTracker(Budgets{Task: 1})
	_ = tr.Record("web", "t1", agent.RoleWorker, stats("/prev", 1.5, "m"))
	c := f.common()
	c.Tracker = tr
	res, err := RunWorker(context.Background(), WorkerOptions{Common: c, Space: "web", Task: "t1", Brief: "b"})
	if !errors.Is(err, ErrBudgetExceeded) || res != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if _, serr := os.Stat(filepath.Join(f.out, "args")); !os.IsNotExist(serr) {
		t.Error("pi was launched despite exceeded budget")
	}
}

func TestRunWorkerTimeout(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	f := newFake(t)
	cfg := agent.DefaultConfig(agent.RoleWorker)
	cfg.Timeout = 300 * time.Millisecond
	c := f.common("FAKE_MODE=sleep")
	c.Config = cfg

	start := time.Now()
	res, err := RunWorker(context.Background(), WorkerOptions{Common: c, Space: "web", Task: "t1", Brief: "b"})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Outcome != agent.OutcomeFailed {
		t.Errorf("res = %+v", res)
	}
	if time.Since(start) > 8*time.Second {
		t.Error("timeout did not terminate the process promptly")
	}
}

func TestRunWorkerCancelled(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(filepath.Join(f.out, "started")); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	res, err := RunWorker(ctx, WorkerOptions{Common: f.common("FAKE_MODE=sleep"), Space: "web", Task: "t1", Brief: "b"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Outcome != agent.OutcomeCancelled {
		t.Errorf("res = %+v", res)
	}
}

func TestRunWorkerBinaryMissing(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	c := Common{Binary: filepath.Join(t.TempDir(), "no-such-pi"), Env: []string{"PATH=/usr/bin"}}
	res, err := RunWorker(context.Background(), WorkerOptions{Common: c, Space: "web", Task: "t1", Brief: "b"})
	if err == nil || res != nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Error("start failure must not look like an exit error")
	}
}

// ---------------------------------------------------------------- Reviewer

const approveJSON = `{"verdict":"approve","summary":"ok","criteria":[{"criterion":"c1","met":true}],"issues":[]}`

func TestBuildReviewerCommand(t *testing.T) {
	d := xdg(t)
	wt := setupWorktree(t, "web", "t1", "invariants here")
	f := newFake(t)
	diff := "diff --git a/f b/f\n+" + strings.Repeat("x", 300*1024) + "\n" // far above typical argv comfort
	cmd, targets, err := BuildReviewerCommand(ReviewerOptions{
		Common: f.common(), Space: "web", Task: "t1", Criteria: []string{"c1"}, Diff: diff,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := cmd.Args
	if flagValue(t, a, "--model") != "claude-opus-5-5" || flagValue(t, a, "--thinking") != "high" {
		t.Errorf("model/thinking: %q", a)
	}
	if got := flagValue(t, a, "--tools"); got != "read,grep,find,ls" {
		t.Errorf("tools = %q", got)
	}
	if !strings.Contains(flagValue(t, a, "--skill"), agent.ReviewerSkillName) {
		t.Errorf("skill = %q, want containing %s", flagValue(t, a, "--skill"), agent.ReviewerSkillName)
	}
	for _, flag := range []string{"--no-extensions", "--no-skills", "--no-context-files", "--no-mcp", "--print"} {
		if !hasFlag(a, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	for _, arg := range a {
		if len(arg) > 4096 {
			t.Errorf("argument of %d bytes: diff must travel over stdin", len(arg))
		}
	}
	if !strings.Contains(cmd.Stdin, "1. c1") || !strings.Contains(cmd.Stdin, "+xxx") || !strings.Contains(cmd.Stdin, `"verdict"`) {
		t.Error("stdin lacks criteria/diff/schema")
	}
	if !strings.Contains(flagValue(t, a, "--append-system-prompt"), "invariants here") {
		t.Error("repo AGENTS.md missing from reviewer system prompt")
	}
	if cmd.Dir != wt {
		t.Errorf("dir = %q", cmd.Dir)
	}
	if targets.Worktree != "" {
		t.Error("reviewer must not own the worktree")
	}
	if targets.SessionDir != filepath.Join(d, "fleet", "sessions", "web", "tasks", "t1", "reviewer") {
		t.Errorf("session dir = %q", targets.SessionDir)
	}
	if !slices.Contains(cmd.Env, "FLEET_ROLE=reviewer") {
		t.Error("FLEET_ROLE missing")
	}
}

func TestBuildReviewerCommandDirFallbackAndErrors(t *testing.T) {
	xdg(t)
	f := newFake(t)
	ok := ReviewerOptions{Common: f.common(), Space: "web", Task: "nowt", Criteria: []string{"c"}, Diff: "d"}
	cmd, _, err := BuildReviewerCommand(ok)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != "" {
		t.Errorf("dir = %q, want inherited cwd when worktree is gone", cmd.Dir)
	}
	o := ok
	o.Worktree = t.TempDir()
	if _, _, err := BuildReviewerCommand(o); !errors.Is(err, ErrOutsideWorktreeRoot) {
		t.Errorf("outside root: %v", err)
	}
	o = ok
	o.Criteria = nil
	if _, _, err := BuildReviewerCommand(o); !errors.Is(err, agent.ErrInvalidPrompt) {
		t.Errorf("no criteria: %v", err)
	}
	o = ok
	o.Task = "../x"
	if _, _, err := BuildReviewerCommand(o); !errors.Is(err, agent.ErrInvalidName) {
		t.Errorf("bad task: %v", err)
	}
	o = ok
	o.Config = agent.DefaultConfig(agent.RoleWorker)
	if _, _, err := BuildReviewerCommand(o); err == nil {
		t.Error("wrong-role config accepted")
	}
}

func TestRunReviewer(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	f := newFake(t)
	tr := NewTracker(Budgets{})
	c := f.common("FAKE_STDOUT=Here is my review:\n```json\n"+approveJSON+"\n```", "FAKE_SESSION="+sessionFile(t, 0.9))
	c.Tracker = tr
	opts := ReviewerOptions{Common: c, Space: "web", Task: "t1", Criteria: []string{"c1"}, Diff: "diff --git a b\n+x"}

	v, res, err := RunReviewer(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Approved() || v.Summary != "ok" {
		t.Errorf("verdict = %+v", v)
	}
	if got := f.read(t, "stdin"); !strings.Contains(got, "+x") || !strings.Contains(got, "1. c1") {
		t.Errorf("fake pi stdin = %q", got)
	}
	if !approx(tr.TaskUsage("web", "t1").Cost.Total, 0.9) || res.Role != agent.RoleReviewer {
		t.Errorf("accounting: %+v", res)
	}
	// Reviewer cleanup removes only its session, never the worktree.
	if err := res.Cleanup(agent.Cleaner{}); err != nil {
		t.Fatal(err)
	}
	wt, _ := agent.WorktreeDir("web", "t1")
	if _, err := os.Stat(wt); err != nil {
		t.Error("reviewer cleanup removed the worktree")
	}
	if _, err := os.Stat(res.Targets.SessionDir); !os.IsNotExist(err) {
		t.Error("reviewer session not cleaned")
	}
}

func TestRunReviewerBadOutput(t *testing.T) {
	xdg(t)
	setupWorktree(t, "web", "t1", "")
	f := newFake(t)
	tr := NewTracker(Budgets{})
	c := f.common("FAKE_STDOUT=LGTM!", "FAKE_SESSION="+sessionFile(t, 0.3))
	c.Tracker = tr
	v, res, err := RunReviewer(context.Background(), ReviewerOptions{Common: c, Space: "web", Task: "t1", Criteria: []string{"c"}, Diff: "d"})
	if !errors.Is(err, agent.ErrNoVerdict) || v != nil {
		t.Fatalf("v=%v err=%v", v, err)
	}
	if res == nil || res.Outcome != agent.OutcomeFailed {
		t.Errorf("res = %+v", res)
	}
	if !approx(tr.TaskUsage("web", "t1").Cost.Total, 0.3) {
		t.Error("spend of an unparsable review must still be recorded")
	}
	// The failed review's session is kept for inspection.
	if err := res.Cleanup(agent.Cleaner{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.Targets.SessionDir); err != nil {
		t.Error("failed reviewer session removed")
	}
}

func TestRunReviewerPiFailure(t *testing.T) {
	xdg(t)
	f := newFake(t)
	v, res, err := RunReviewer(context.Background(), ReviewerOptions{
		Common: f.common("FAKE_EXIT=1"), Space: "web", Task: "t1", Criteria: []string{"c"}, Diff: "d",
	})
	var ee *ExitError
	if !errors.As(err, &ee) || v != nil || res == nil {
		t.Fatalf("v=%v res=%v err=%v", v, res, err)
	}
}

// -------------------------------------------------------------------- Lead

func TestBuildLeadCommandInteractive(t *testing.T) {
	d := xdg(t)
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("lead sees this"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	cmd, err := BuildLeadCommand(LeadOptions{Common: f.common("TOPH_X=1"), Space: "web", Summary: "Web app", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	a := cmd.Args
	skillDir := filepath.Join(d, "fleet", "skills", "fleet-lead")
	if flagValue(t, a, "--skill") != skillDir {
		t.Errorf("skill = %q", flagValue(t, a, "--skill"))
	}
	if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Errorf("skill not materialised: %v", err)
	}
	if flagValue(t, a, "--model") != "haiku" || flagValue(t, a, "--thinking") != "low" {
		t.Errorf("tier: %q", a)
	}
	if flagValue(t, a, "--session-id") != "fleet-lead-web" || flagValue(t, a, "--name") != "lead:web" {
		t.Errorf("session identity: %q", a)
	}
	wantSessions := filepath.Join(d, "fleet", "sessions", "web", "lead")
	if flagValue(t, a, "--session-dir") != wantSessions {
		t.Errorf("session-dir = %q", flagValue(t, a, "--session-dir"))
	}
	if fi, err := os.Stat(wantSessions); err != nil || !fi.IsDir() {
		t.Error("lead session dir not created")
	}
	if hasFlag(a, "--mode") || hasFlag(a, "--print") {
		t.Errorf("interactive lead must open the TUI: %q", a)
	}
	if !hasFlag(a, "--no-skills") { // ambient skills (like the user's own fleet skill) stay out; only --skill loads
		t.Error("ambient skills not disabled")
	}
	if a[len(a)-2] != "--" || !strings.Contains(a[len(a)-1], `space "web"`) {
		t.Errorf("initial prompt: %q", a[len(a)-2:])
	}
	if !strings.Contains(flagValue(t, a, "--append-system-prompt"), "lead sees this") {
		t.Error("repo AGENTS.md missing")
	}
	if cmd.Dir != repo {
		t.Errorf("dir = %q", cmd.Dir)
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "TOPH_X") {
		t.Error("TOPH_ env leaked")
	}
	if !slices.Contains(cmd.Env, "FLEET_ROLE=lead") || !slices.Contains(cmd.Env, "FLEET_SPACE=web") {
		t.Error("fleet env missing")
	}

	// Custom prompt and skill path.
	cmd, err = BuildLeadCommand(LeadOptions{Common: f.common(), Space: "web", SkillPath: "/my/skill", Prompt: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	if flagValue(t, cmd.Args, "--skill") != "/my/skill" || cmd.Args[len(cmd.Args)-1] != "custom" {
		t.Errorf("overrides ignored: %q", cmd.Args)
	}
}

func TestBuildLeadCommandHeadless(t *testing.T) {
	xdg(t)
	cmd, err := BuildLeadCommand(LeadOptions{Common: newFake(t).common(), Space: "web", Headless: true, Prompt: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if flagValue(t, cmd.Args, "--mode") != "rpc" {
		t.Errorf("args = %q", cmd.Args)
	}
	if slices.Contains(cmd.Args, "--") || slices.Contains(cmd.Args, "ignored") {
		t.Errorf("headless lead must receive prompts over RPC, not argv: %q", cmd.Args)
	}
}

func TestBuildLeadCommandErrors(t *testing.T) {
	xdg(t)
	c := newFake(t).common()
	if _, err := BuildLeadCommand(LeadOptions{Common: c, Space: "../x"}); !errors.Is(err, agent.ErrInvalidName) {
		t.Errorf("bad space: %v", err)
	}
	if _, err := BuildLeadCommand(LeadOptions{Common: c, Space: "web", Dir: "/definitely/not/here"}); err == nil {
		t.Error("missing dir accepted")
	}
	bad := c
	bad.Config = agent.DefaultConfig(agent.RoleLead)
	bad.Config.Persistent = false
	if _, err := BuildLeadCommand(LeadOptions{Common: bad, Space: "web"}); err == nil {
		t.Error("non-persistent lead accepted")
	}
	// The lead may opt in to user context; isolation flags then disappear.
	inh := c
	inh.Config = agent.DefaultConfig(agent.RoleLead)
	inh.Config.InheritUserContext = true
	cmd, err := BuildLeadCommand(LeadOptions{Common: inh, Space: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(cmd.Args, "--no-extensions") {
		t.Error("inherit flag ignored")
	}
}

func TestStartLeadRPC(t *testing.T) {
	xdg(t)
	f := newFake(t)
	tr := NewTracker(Budgets{Space: 1})
	c := f.common("FAKE_MODE=rpc")
	c.Tracker = tr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	p, err := StartLead(ctx, LeadOptions{Common: c, Space: "web"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(time.Second)
	if p.PID() <= 0 {
		t.Error("no pid")
	}

	ev := nextEvent(t, p)
	if ev.Type != "echo" || !strings.Contains(string(ev.Raw), `"type":"prompt"`) || !strings.Contains(string(ev.Raw), `space \"web\"`) {
		t.Errorf("initial prompt event = %s", ev.Raw)
	}
	if err := p.PromptSteer("hurry"); err != nil {
		t.Fatal(err)
	}
	ev = nextEvent(t, p)
	if !strings.Contains(string(ev.Raw), `"streamingBehavior":"steer"`) || !strings.Contains(string(ev.Raw), "hurry") {
		t.Errorf("steer = %s", ev.Raw)
	}
	_ = p.PromptFollowUp("later")
	if ev = nextEvent(t, p); !strings.Contains(string(ev.Raw), `"followUp"`) {
		t.Errorf("follow-up = %s", ev.Raw)
	}
	_ = p.Abort()
	if ev = nextEvent(t, p); !strings.Contains(string(ev.Raw), `"type":"abort"`) {
		t.Errorf("abort = %s", ev.Raw)
	}
	if !strings.Contains(strings.Join(f.argsOrNil(), " "), "--mode rpc") {
		t.Errorf("fake pi args = %q", f.argsOrNil())
	}

	if err := p.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("Done not closed after Stop")
	}
	for range p.Events() { // channel must be closed
	}
	if err := p.Send(map[string]any{"type": "abort"}); err == nil {
		t.Error("Send after exit should fail")
	}
	if err := p.Stop(time.Second); err != nil { // idempotent
		t.Error(err)
	}
}

func (f *fake) argsOrNil() []string {
	b, err := os.ReadFile(filepath.Join(f.out, "args"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func nextEvent(t *testing.T, p *Process) Event {
	t.Helper()
	select {
	case ev, ok := <-p.Events():
		if !ok {
			t.Fatalf("events closed; stderr: %s", p.Stderr())
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return Event{}
}

func TestStartLeadBudgetBlocks(t *testing.T) {
	xdg(t)
	f := newFake(t)
	tr := NewTracker(Budgets{Space: 1})
	_ = tr.Record("web", "", agent.RoleLead, stats("/l", 2, "m"))
	c := f.common("FAKE_MODE=rpc")
	c.Tracker = tr
	if p, err := StartLead(context.Background(), LeadOptions{Common: c, Space: "web"}); !errors.Is(err, ErrBudgetExceeded) || p != nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
}

func TestProcessStopKillsStubbornProcess(t *testing.T) {
	// A process that ignores SIGTERM is killed after the grace period.
	dir := t.TempDir()
	script := filepath.Join(dir, "stubborn")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\necho ready\nwhile true; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := StartProcess(context.Background(), Command{Path: script, Env: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEventRaw(t, p); ev != "ready" {
		t.Logf("first line: %q", ev)
	}
	pid := p.PID()
	start := time.Now()
	_ = p.Stop(300 * time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Error("Stop did not escalate to SIGKILL")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		// zombie-free check: process must be gone and reaped
		t.Error("process still alive after Stop")
	}
	if err := p.Wait(); err == nil {
		t.Error("killed process should report an error from Wait")
	}
}

func nextEventRaw(t *testing.T, p *Process) string {
	t.Helper()
	select {
	case ev, ok := <-p.Events():
		if !ok {
			return ""
		}
		return string(ev.Raw)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	return ""
}

func TestProcessEventFraming(t *testing.T) {
	// LF-only framing: U+2028 inside a JSON string must not split a record,
	// CRLF is tolerated, non-JSON lines surface with an empty Type.
	dir := t.TempDir()
	script := filepath.Join(dir, "emit")
	body := "#!/bin/sh\nprintf '{\"type\":\"a\",\"s\":\"x\xe2\x80\xa8y\"}\\r\\n'\nprintf 'garbage\\n'\nprintf '{\"type\":\"b\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := StartProcess(context.Background(), Command{Path: script, Env: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for ev := range p.Events() {
		got = append(got, ev)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %v", got)
	}
	if got[0].Type != "a" || !strings.Contains(string(got[0].Raw), "x\u2028y") || strings.HasSuffix(string(got[0].Raw), "\r") {
		t.Errorf("first = %q type %q", got[0].Raw, got[0].Type)
	}
	if got[1].Type != "" || string(got[1].Raw) != "garbage" {
		t.Errorf("second = %+v", got[1])
	}
	if got[2].Type != "b" {
		t.Errorf("unterminated last record = %+v", got[2])
	}
}

func TestProcessContextCancelTerminates(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := StartProcess(ctx, Command{Path: f.bin, Env: f.env("FAKE_MODE=sleep")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(f.out, "started")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process survived context cancel")
	}
	// A blocked consumer must not leak the reader goroutine after Stop.
	_ = p.Stop(time.Second)
}

func TestStartProcessMissingBinary(t *testing.T) {
	if _, err := StartProcess(context.Background(), Command{Path: "/no/such/pi"}); err == nil {
		t.Fatal("expected start error")
	}
}

func TestCommandExecSetsProcessGroup(t *testing.T) {
	cmd := Command{Path: "true", Dir: "/tmp", Env: []string{"A=1"}, Stdin: "in"}.Exec(context.Background())
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.Dir != "/tmp" || cmd.Stdin == nil {
		t.Errorf("cmd = %+v", cmd)
	}
	var _ *exec.Cmd = cmd // usable with tea.ExecProcess
}

// ------------------------------------------------------------------- Misc

func TestBuildEnv(t *testing.T) {
	env := buildEnv([]string{"A=1", "TOPH_MEMORY=x", "PI_MODEL=m", "PI_PROVIDER=p", "PI_SESSION_ID=s", "PI_OFFLINE=1", "B=a=b"},
		map[string]string{"FLEET_ROLE": "worker", "FLEET_TASK": ""})
	want := []string{"A=1", "PI_OFFLINE=1", "B=a=b", "FLEET_ROLE=worker"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
	if got := buildEnv(nil, nil); len(got) == 0 {
		t.Error("nil base should fall back to os.Environ()")
	}
}

func TestLeadSessionID(t *testing.T) {
	if LeadSessionID("web") != "fleet-lead-web" {
		t.Error("session id changed; resumability depends on it")
	}
}

func TestLimitedAndTailBuffers(t *testing.T) {
	lb := &limitedBuffer{max: 5}
	n, _ := lb.Write([]byte("abc"))
	lb.Write([]byte("defgh"))
	if n != 3 || lb.String() != "abcde" {
		t.Errorf("limited = %q", lb.String())
	}
	tb := &tailBuffer{max: 4}
	tb.Write([]byte("ab"))
	tb.Write([]byte("cdef"))
	if tb.String() != "cdef" {
		t.Errorf("tail = %q", tb.String())
	}
}
