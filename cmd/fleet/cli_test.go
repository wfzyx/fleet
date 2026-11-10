package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePi is a stand-in for the pi binary in RPC mode: every prompt gets the
// reply "reply N" where N counts the prompts received so far.
const fakePi = `#!/bin/sh
n=0
while IFS= read -r line; do
  case "$line" in
    *'"type":"prompt"'*)
      n=$((n+1))
      echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"reply '$n'"}]}}'
      echo '{"type":"agent_settled","aborted":false}'
      ;;
  esac
done
`

type testFleet struct {
	t      *testing.T
	socket string
	repo   string
}

// newTestFleet starts an in-process daemon over temp XDG directories.
func newTestFleet(t *testing.T) *testFleet {
	t.Helper()
	root := t.TempDir()
	rt, err := os.MkdirTemp("", "fl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rt) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", rt)
	t.Setenv("FLEET_SPACE", "")
	t.Setenv("FLEET_PI_BIN", "")

	pi := filepath.Join(root, "pi")
	if err := os.WriteFile(pi, []byte(fakePi), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &testFleet{t: t, socket: filepath.Join(rt, "f.sock"), repo: repo}
	a := &app{socket: f.socket, piBin: pi}
	e, err := a.startService()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.stop(10 * time.Second); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	return f
}

// run executes the CLI against the test daemon.
func (f *testFleet) run(args ...string) (string, error) {
	f.t.Helper()
	return runCLI(append([]string{"--socket", f.socket}, args...)...)
}

func (f *testFleet) mustRun(args ...string) string {
	f.t.Helper()
	out, err := f.run(args...)
	if err != nil {
		f.t.Fatalf("fleet %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func runCLI(args ...string) (string, error) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func TestSpaceLifecycle(t *testing.T) {
	f := newTestFleet(t)
	if out := f.mustRun("space", "list"); !strings.Contains(out, "no spaces") {
		t.Errorf("empty list: %q", out)
	}
	out := f.mustRun("space", "add", "web", f.repo, "--budget", "12.5", "--task-budget", "3", "--summary", "the site")
	if !strings.Contains(out, "added space web") {
		t.Errorf("add: %q", out)
	}
	if _, err := f.run("space", "add", "web", f.repo); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate add: %v", err)
	}
	if _, err := f.run("space", "add", "bad", f.repo, "--project", "nopath"); err == nil || !strings.Contains(err.Error(), "id=path") {
		t.Errorf("bad --project: %v", err)
	}
	f.mustRun("space", "add", "multi", "--project", "api="+f.repo, "--project", "ui="+f.repo)

	out = f.mustRun("space", "list")
	for _, want := range []string{"SPACE", "web", "$12.50", "$3.00", "api,ui", "offline"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	var rows []spaceRow
	if err := json.Unmarshal([]byte(f.mustRun("--json", "space", "ls")), &rows); err != nil || len(rows) != 2 || rows[1].ID != "web" || rows[1].MaxBudgetUSD != 12.5 {
		t.Errorf("json list = %+v, %v", rows, err)
	}

	// Budget: show, change, validate.
	out = f.mustRun("space", "budget", "web", "--max", "20", "--task-max", "0")
	if !strings.Contains(out, "space web") || !strings.Contains(out, "$20.00") {
		t.Errorf("budget: %q", out)
	}
	if out = f.mustRun("space", "list"); !strings.Contains(out, "$20.00") || strings.Contains(out, "$3.00") {
		t.Errorf("budget not applied:\n%s", out)
	}
	if out = f.mustRun("space", "budget", "--fleet", "100"); !strings.Contains(out, "$100.00") {
		t.Errorf("fleet budget: %q", out)
	}
	if _, err := f.run("space", "budget", "web", "--max", "-1"); err == nil {
		t.Error("negative budget accepted")
	}
	if _, err := f.run("space", "budget"); err == nil {
		t.Error("budget without space accepted")
	}
	if _, err := f.run("space", "budget", "--max", "5"); err == nil {
		t.Error("--max without space accepted")
	}
	if _, err := f.run("space", "budget", "nope"); err == nil || !strings.Contains(err.Error(), "no such space") {
		t.Errorf("budget of unknown space: %v", err)
	}

	// Remove: blocked by active tasks unless forced.
	f.mustRun("delegate", "web", "t1", "do it")
	if _, err := f.run("space", "remove", "web"); err == nil || !strings.Contains(err.Error(), "active tasks") {
		t.Errorf("remove with active tasks: %v", err)
	}
	if out = f.mustRun("space", "rm", "web", "--force"); !strings.Contains(out, "removed space web") {
		t.Errorf("force remove: %q", out)
	}
	if _, err := f.run("space", "remove", "web"); err == nil {
		t.Error("removing twice succeeded")
	}
}

func TestTaskCommands(t *testing.T) {
	f := newTestFleet(t)
	f.mustRun("space", "add", "web", f.repo)

	out := f.mustRun("delegate", "web", "fix-bug", "Fix the bug", "--criteria", "tests pass", "--criteria", "no regressions", "--title", "Fix bug")
	if !strings.Contains(out, "delegated fix-bug [queued] Fix bug") {
		t.Errorf("delegate: %q", out)
	}
	if _, err := f.run("delegate", "web", "fix-bug", "again"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate delegate: %v", err)
	}
	if _, err := f.run("delegate", "web", "x", "  "); err == nil {
		t.Error("empty brief accepted")
	}
	if _, err := f.run("delegate", "web", "x"); err == nil {
		t.Error("missing brief accepted")
	}
	// The repo is not a git checkout, so the worker cannot start; the task must survive.
	if _, err := f.run("delegate", "web", "sp", "brief", "--branch", "feat/x"); err == nil || !strings.Contains(err.Error(), "was created but the worker did not start") {
		t.Errorf("delegate --branch: %v", err)
	}
	if _, err := f.run("spawn", "ghost"); err == nil || !strings.Contains(err.Error(), "no such task") {
		t.Errorf("spawn unknown: %v", err)
	}

	// status
	out = f.mustRun("status")
	if !strings.Contains(out, "1 space(s)") || !strings.Contains(out, "web: lead offline") {
		t.Errorf("status: %q", out)
	}
	if _, err := f.run("report", "fix-bug", "progress"); err == nil {
		t.Error("progress without note accepted")
	}
	f.mustRun("report", "fix-bug", "progress", "halfway there")
	f.mustRun("report", "fix-bug", "blocked", "need credentials")
	out = f.mustRun("status", "fix-bug")
	for _, want := range []string{"fix-bug: Fix bug", "queued (blocked)", "tests pass", "no regressions", "need credentials", "halfway there"} {
		if !strings.Contains(out, want) {
			t.Errorf("task status lacks %q:\n%s", want, out)
		}
	}
	var st taskStatus
	if err := json.Unmarshal([]byte(f.mustRun("--json", "status", "fix-bug")), &st); err != nil || !st.Blocked || st.Task.ID != "fix-bug" || len(st.Log) < 3 {
		t.Errorf("json status = %+v, %v", st, err)
	}
	if _, err := f.run("status", "ghost"); err == nil {
		t.Error("status of unknown task succeeded")
	}
	if _, err := f.run("report", "fix-bug", "bogus", "n"); err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Errorf("bogus status: %v", err)
	}

	// brief
	out = f.mustRun("brief")
	if !strings.Contains(out, "web: lead offline") || !strings.Contains(out, "fix-bug") || !strings.Contains(out, "queued!") || !strings.Contains(out, "need credentials") {
		t.Errorf("brief:\n%s", out)
	}
	t.Setenv("FLEET_SPACE", "web")
	if out = f.mustRun("brief"); !strings.Contains(out, "web:") {
		t.Errorf("brief with FLEET_SPACE: %q", out)
	}
	if _, err := f.run("brief", "nope"); err == nil || !strings.Contains(err.Error(), "no such space") {
		t.Errorf("brief unknown space: %v", err)
	}
	var br []briefSpace
	if err := json.Unmarshal([]byte(f.mustRun("--json", "brief", "web")), &br); err != nil || len(br) != 1 || len(br[0].Tasks) != 2 || !br[0].Tasks[0].Blocked {
		t.Errorf("json brief = %+v, %v", br, err)
	}

	// cost
	if out = f.mustRun("cost", "fix-bug"); !strings.Contains(out, "space web task fix-bug") {
		t.Errorf("cost: %q", out)
	}
	if out = f.mustRun("cost"); !strings.Contains(out, "space web:") {
		t.Errorf("cost via FLEET_SPACE: %q", out)
	}
	t.Setenv("FLEET_SPACE", "")
	if _, err := f.run("cost"); err == nil {
		t.Error("cost without scope accepted")
	}

	// review / merge on a task that is not in review fail with a state error.
	if _, err := f.run("review", "fix-bug"); err == nil || !strings.Contains(err.Error(), "cannot review") {
		t.Errorf("review: %v", err)
	}
	if _, err := f.run("merge", "fix-bug"); err == nil || !strings.Contains(err.Error(), "cannot merge") {
		t.Errorf("merge: %v", err)
	}

	// close
	out = f.mustRun("close", "fix-bug", "not needed")
	if !strings.Contains(out, "closed fix-bug [closed]") {
		t.Errorf("close: %q", out)
	}
	if _, err := f.run("close", "fix-bug"); err == nil {
		t.Error("closing twice succeeded")
	}
	if out = f.mustRun("brief", "web"); strings.Contains(out, "fix-bug") {
		t.Errorf("closed task still in brief:\n%s", out)
	}
}

func TestPingReachesLead(t *testing.T) {
	f := newTestFleet(t)
	f.mustRun("space", "add", "web", f.repo)
	// First prompt is the lead's intro turn ("reply 1"); the ping is the second.
	if out := f.mustRun("ping", "web", "status?"); strings.TrimSpace(out) != "reply 2" {
		t.Errorf("ping reply = %q", out)
	}
	if out := f.mustRun("space", "list"); !strings.Contains(out, "idle") {
		t.Errorf("lead should be idle:\n%s", out)
	}
	var res struct{ Reply string }
	if err := json.Unmarshal([]byte(f.mustRun("--json", "ping", "web", "again")), &res); err != nil || res.Reply != "reply 3" {
		t.Errorf("json ping = %+v, %v", res, err)
	}
	if _, err := f.run("ping", "nope", "hi"); err == nil || !strings.Contains(err.Error(), "no such space") {
		t.Errorf("ping unknown space: %v", err)
	}
	if _, err := f.run("ping", "web", "  "); err == nil {
		t.Error("blank ping accepted")
	}
}

func TestNoDaemonHint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", root)
	_, err := runCLI("--socket", filepath.Join(root, "none.sock"), "status")
	if err == nil || !strings.Contains(err.Error(), "fleet daemon") {
		t.Errorf("error without daemon = %v", err)
	}
}

func TestArgValidation(t *testing.T) {
	for _, args := range [][]string{
		{"spawn"}, {"spawn", "a", "b", "c"}, {"report", "t"}, {"review"}, {"merge", "a", "b"},
		{"close"}, {"ping", "web"}, {"delegate", "a", "b"}, {"daemon", "extra"}, {"space", "add"},
		{"space", "remove"}, {"bogus"},
	} {
		if _, err := runCLI(append([]string{"--socket", "/nonexistent/x.sock"}, args...)...); err == nil || strings.Contains(err.Error(), "start it with") {
			t.Errorf("fleet %v: want a usage error, got %v", args, err)
		}
	}
}

func TestDaemonServesUntilCancelled(t *testing.T) {
	root := t.TempDir()
	rt, err := os.MkdirTemp("", "fl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(rt)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", rt)
	sock := filepath.Join(rt, "d.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--socket", sock, "daemon", "--grace", "5s"})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if o, err := runCLI("--socket", sock, "status"); err == nil {
			if !strings.Contains(o, "0 space(s)") {
				t.Errorf("status: %q", o)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A second daemon on the same socket must refuse to start.
	if _, err := runCLI("--socket", sock, "daemon"); err == nil || !strings.Contains(err.Error(), "another fleet server") {
		t.Errorf("second daemon: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
}
