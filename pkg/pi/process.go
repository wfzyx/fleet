package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"fleet/pkg/agent"
)

// DefaultBinary is the Pi executable looked up on PATH.
const DefaultBinary = "pi"

const (
	// maxOutputBytes caps captured stdout of one-shot runs.
	maxOutputBytes = 8 << 20
	// stderrTailBytes is how much stderr is kept for diagnostics.
	stderrTailBytes = 8 << 10
	// maxContextFileBytes caps the AGENTS.md injected into a prompt.
	maxContextFileBytes = 64 << 10
	// killGrace is how long a terminated process may take before SIGKILL.
	killGrace = 10 * time.Second
)

// Errors returned by command builders.
var (
	ErrOutsideWorktreeRoot = errors.New("pi: worktree is outside the fleet worktree root")
	ErrWorktreeMissing     = errors.New("pi: worktree does not exist")
)

// Command is a fully resolved Pi invocation.
type Command struct {
	Path  string
	Args  []string
	Dir   string
	Env   []string
	Stdin string // optional; piped to Pi, which prepends it to the first prompt
}

// Exec returns an *exec.Cmd for c bound to ctx. The process runs in its own
// process group; cancelling ctx sends SIGTERM to the group, then SIGKILL
// after a grace period. The result also works with tea.ExecProcess.
func (c Command) Exec(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = killGrace
	return cmd
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return cmd.Process.Signal(sig)
	}
	return nil
}

// Common holds options shared by all roles.
type Common struct {
	// Binary is the Pi executable; defaults to DefaultBinary.
	Binary string
	// Config overrides the role's agent.Config. The zero value selects the
	// role default.
	Config agent.Config
	// Env is the base environment; nil means os.Environ().
	Env []string
	// Extensions are extension paths loaded with -e even though ambient
	// extension discovery is disabled. Use this for model-provider extensions
	// (for example a Claude bridge) that Pi needs to reach the configured model.
	Extensions []string
	// Tracker, when set, enforces budgets before a run and records session
	// spend after it.
	Tracker *Tracker
}

func (c Common) binary() string {
	if c.Binary != "" {
		return c.Binary
	}
	return DefaultBinary
}

func (c Common) config(role agent.Role) (agent.Config, error) {
	cfg := c.Config
	if cfg.Role == "" {
		cfg = agent.DefaultConfig(role)
	}
	if cfg.Role != role {
		return cfg, fmt.Errorf("pi: config is for role %q, want %q", cfg.Role, role)
	}
	return cfg, cfg.Validate()
}

// scrubbedPrefixes and scrubbedKeys are removed from child environments so a role
// cannot reach the user's memory stores or a parent's session.
var (
	scrubbedPrefixes = []string{"TOPH_"}
	scrubbedKeys     = []string{
		"PI_SESSION_ID", "PI_SESSION_FILE", "PI_PROVIDER", "PI_MODEL",
		"PI_REASONING_LEVEL", "PI_CODING_AGENT_SESSION_DIR",
	}
)

// buildEnv scrubs base and appends the fleet variables.
func buildEnv(base []string, vars map[string]string) []string {
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+len(vars))
outer:
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		for _, p := range scrubbedPrefixes {
			if strings.HasPrefix(k, p) {
				continue outer
			}
		}
		for _, s := range scrubbedKeys {
			if k == s {
				continue outer
			}
		}
		out = append(out, kv)
	}
	for _, k := range sortedKeys(vars) {
		if vars[k] != "" {
			out = append(out, k+"="+vars[k])
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// baseArgs builds the flags shared by all roles. Unless the config inherits
// user context, ambient extensions, skills, prompt templates, context files
// and MCP are disabled: that is what keeps episodic memory and dream files out.
func baseArgs(c Common, cfg agent.Config, sessionDir, systemPrompt string) []string {
	var a []string
	a = append(a, "--model", cfg.ModelPattern())
	if cfg.Thinking != "" {
		a = append(a, "--thinking", cfg.Thinking)
	}
	a = append(a, "--session-dir", sessionDir)
	if len(cfg.Tools) > 0 {
		a = append(a, "--tools", strings.Join(cfg.Tools, ","))
	}
	if !cfg.InheritUserContext {
		a = append(a, "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-mcp")
		for _, e := range c.Extensions {
			a = append(a, "-e", e)
		}
	}
	if systemPrompt != "" {
		a = append(a, "--append-system-prompt", systemPrompt)
	}
	return a
}

// repoContext returns the repository's AGENTS.md from dir as a system-prompt
// section, or "" when absent. Context-file discovery is disabled for isolated
// roles, so this is the only repository guidance they receive.
func repoContext(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	f, err := os.Open(dir + string(os.PathSeparator) + "AGENTS.md")
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pi: read AGENTS.md: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxContextFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("pi: read AGENTS.md: %w", err)
	}
	text := strings.TrimSpace(string(data))
	if len(data) > maxContextFileBytes {
		text = strings.TrimSpace(string(data[:maxContextFileBytes])) + "\n[AGENTS.md truncated]"
	}
	if text == "" {
		return "", nil
	}
	return "# Repository AGENTS.md\n\n" + text, nil
}

func joinPrompt(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, strings.TrimSpace(p))
		}
	}
	return strings.Join(keep, "\n\n")
}

// ---------------------------------------------------------------- Lead

// LeadOptions configures a lead process.
type LeadOptions struct {
	Common
	Space   string
	Summary string
	// Dir is the space's working directory (usually the main checkout).
	Dir string
	// SessionDir overrides the default agent.SessionDir(lead, space).
	SessionDir string
	// SkillPath overrides the fleet-lead skill; by default the embedded skill
	// is materialised under $XDG_DATA_HOME/fleet/skills.
	SkillPath string
	// Headless runs `pi --mode rpc` for StartLead; otherwise Pi opens its TUI.
	Headless bool
	// Prompt is the first message for interactive mode. Empty uses the
	// rendered lead prompt. Ignored when Headless (send it with Process.Prompt).
	Prompt string
}

// LeadSessionID is the stable session id that makes the lead resumable.
func LeadSessionID(space string) string { return "fleet-lead-" + space }

// BuildLeadCommand resolves the lead invocation. The lead's session persists
// in a per-space directory and is resumed by id on every start. As a side
// effect it creates the session directory and materialises the lead skill.
func BuildLeadCommand(o LeadOptions) (Command, error) {
	cfg, err := o.config(agent.RoleLead)
	if err != nil {
		return Command{}, err
	}
	if err := agent.ValidateName("space", o.Space); err != nil {
		return Command{}, err
	}
	if o.Dir != "" {
		if fi, err := os.Stat(o.Dir); err != nil || !fi.IsDir() {
			return Command{}, fmt.Errorf("pi: lead dir %q is not a directory", o.Dir)
		}
	}
	sessionDir := o.SessionDir
	if sessionDir == "" {
		if sessionDir, err = agent.SessionDir(agent.RoleLead, o.Space, ""); err != nil {
			return Command{}, err
		}
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return Command{}, fmt.Errorf("pi: create session dir: %w", err)
	}
	skill := o.SkillPath
	if skill == "" {
		if skill, err = agent.EnsureLeadSkill(); err != nil {
			return Command{}, err
		}
	}
	prompt, err := agent.RenderLeadPrompt(agent.LeadPromptData{Space: o.Space, Summary: o.Summary, Dir: o.Dir})
	if err != nil {
		return Command{}, err
	}
	repo, err := repoContext(o.Dir)
	if err != nil {
		return Command{}, err
	}

	args := baseArgs(o.Common, cfg, sessionDir, joinPrompt(agent.LeadSystemPrompt, repo))
	args = append(args, "--skill", skill, "--session-id", LeadSessionID(o.Space), "--name", "lead:"+o.Space)
	if o.Headless {
		args = append(args, "--mode", "rpc")
	} else {
		if o.Prompt != "" {
			prompt = o.Prompt
		}
		args = append(args, "--", prompt)
	}
	return Command{
		Path: o.binary(), Args: args, Dir: o.Dir,
		Env: buildEnv(o.Env, map[string]string{"FLEET_ROLE": string(agent.RoleLead), "FLEET_SPACE": o.Space}),
	}, nil
}

// RunInteractive runs c attached to the calling terminal until Pi exits.
func RunInteractive(ctx context.Context, c Command) error {
	cmd := c.Exec(ctx)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// StartLead launches a headless (RPC) lead and sends its initial prompt.
// It enforces budgets first when a Tracker is configured.
func StartLead(ctx context.Context, o LeadOptions) (*Process, error) {
	o.Headless = true
	if o.Tracker != nil {
		if err := o.Tracker.Enforce(o.Space, ""); err != nil {
			return nil, err
		}
	}
	cmd, err := BuildLeadCommand(o)
	if err != nil {
		return nil, err
	}
	p, err := StartProcess(ctx, cmd)
	if err != nil {
		return nil, err
	}
	first := o.Prompt
	if first == "" {
		first, _ = agent.RenderLeadPrompt(agent.LeadPromptData{Space: o.Space, Summary: o.Summary, Dir: o.Dir})
	}
	if err := p.Prompt(first); err != nil {
		_ = p.Stop(time.Second)
		return nil, err
	}
	return p, nil
}

// ---------------------------------------------------------------- Worker

// WorkerOptions configures a worker run.
type WorkerOptions struct {
	Common
	Space string
	Task  string
	Title string
	// Brief is the task description; it is the worker's only instruction.
	Brief      string
	Branch     string
	BaseBranch string
	Acceptance []string
	// Worktree overrides agent.WorktreeDir(space, task). It must live under
	// the fleet worktree root.
	Worktree string
	// SessionDir overrides agent.SessionDir(worker, space, task).
	SessionDir string
}

// BuildWorkerCommand resolves the one-shot worker invocation. The worker runs
// in the XDG worktree with ambient extensions, skills, prompt templates,
// context files and MCP disabled, so user episodic memory and dream files are
// unreachable; its only context is the task prompt and the worktree's AGENTS.md.
func BuildWorkerCommand(o WorkerOptions) (Command, agent.Targets, error) {
	cfg, err := o.config(agent.RoleWorker)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	wt, err := resolveWorktree(o.Space, o.Task, o.Worktree)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	sessionDir, err := resolveSessionDir(agent.RoleWorker, o.Space, o.Task, o.SessionDir)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	prompt, err := agent.RenderWorkerPrompt(agent.WorkerPromptData{
		Space: o.Space, Task: o.Task, Title: o.Title, Brief: o.Brief,
		Branch: o.Branch, BaseBranch: o.BaseBranch, Worktree: wt, Acceptance: o.Acceptance,
	})
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	repo, err := repoContext(wt)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	args := baseArgs(o.Common, cfg, sessionDir, joinPrompt(agent.WorkerSystemPrompt, repo))
	args = append(args, "--print", "--mode", "text", "--name", "worker:"+o.Space+"/"+o.Task, "--", prompt)
	return Command{
		Path: o.binary(), Args: args, Dir: wt,
		Env: buildEnv(o.Env, map[string]string{
			"FLEET_ROLE": string(agent.RoleWorker), "FLEET_SPACE": o.Space, "FLEET_TASK": o.Task, "FLEET_WORKTREE": wt,
		}),
	}, agent.Targets{Worktree: wt, SessionDir: sessionDir}, nil
}

func resolveWorktree(space, task, override string) (string, error) {
	wt := override
	if wt == "" {
		var err error
		if wt, err = agent.WorktreeDir(space, task); err != nil {
			return "", err
		}
	} else if err := firstErr(agent.ValidateName("space", space), agent.ValidateName("task", task)); err != nil {
		return "", err
	}
	root, err := agent.WorktreeRoot()
	if err != nil {
		return "", err
	}
	if !agent.Within(root, wt) {
		return "", fmt.Errorf("%w: %s (root %s)", ErrOutsideWorktreeRoot, wt, root)
	}
	fi, err := os.Stat(wt)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrWorktreeMissing, wt)
	}
	return wt, nil
}

func resolveSessionDir(role agent.Role, space, task, override string) (string, error) {
	dir := override
	if dir == "" {
		var err error
		if dir, err = agent.SessionDir(role, space, task); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("pi: create session dir: %w", err)
	}
	return dir, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ---------------------------------------------------------------- Reviewer

// ReviewerOptions configures a reviewer run.
type ReviewerOptions struct {
	Common
	Space    string
	Task     string
	Title    string
	Criteria []string
	// Diff is the git diff under review.
	Diff string
	// Worktree is where the reviewer may read surrounding code. It defaults to
	// agent.WorktreeDir(space, task) when that exists, else the process's cwd.
	Worktree   string
	SessionDir string
	// SkillPath overrides the directory containing SKILL.md. When empty,
	// agent.EnsureReviewerSkill materialises the embedded reviewer skill.
	SkillPath string
}

// BuildReviewerCommand resolves the one-shot reviewer invocation. The review
// prompt (criteria + diff) is piped over stdin so large diffs never hit the
// argv size limit; the reviewer is read-only by tool allowlist.
func BuildReviewerCommand(o ReviewerOptions) (Command, agent.Targets, error) {
	cfg, err := o.config(agent.RoleReviewer)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	dir := ""
	switch {
	case o.Worktree != "":
		if dir, err = resolveWorktree(o.Space, o.Task, o.Worktree); err != nil {
			return Command{}, agent.Targets{}, err
		}
	default:
		if d, derr := agent.WorktreeDir(o.Space, o.Task); derr == nil {
			if fi, serr := os.Stat(d); serr == nil && fi.IsDir() {
				dir = d
			}
		} else {
			return Command{}, agent.Targets{}, derr
		}
	}
	sessionDir, err := resolveSessionDir(agent.RoleReviewer, o.Space, o.Task, o.SessionDir)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	prompt, err := agent.RenderReviewerPrompt(agent.ReviewPromptData{
		Space: o.Space, Task: o.Task, Title: o.Title, Criteria: o.Criteria, Diff: o.Diff,
	}, cfg.MaxDiffBytes)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	repo, err := repoContext(dir)
	if err != nil {
		return Command{}, agent.Targets{}, err
	}
	skill := o.SkillPath
	if skill == "" {
		if skill, err = agent.EnsureReviewerSkill(); err != nil {
			return Command{}, agent.Targets{}, err
		}
	}
	args := baseArgs(o.Common, cfg, sessionDir, joinPrompt(agent.ReviewerSystemPrompt, repo))
	args = append(args, "--skill", skill, "--print", "--mode", "text", "--name", "review:"+o.Space+"/"+o.Task,
		"--", "Review the change described above and reply with the JSON verdict only.")
	return Command{
		Path: o.binary(), Args: args, Dir: dir, Stdin: prompt,
		Env: buildEnv(o.Env, map[string]string{
			"FLEET_ROLE": string(agent.RoleReviewer), "FLEET_SPACE": o.Space, "FLEET_TASK": o.Task,
		}),
	}, agent.Targets{SessionDir: sessionDir}, nil
}

// ---------------------------------------------------------------- One-shot runs

// RunResult is the outcome of a one-shot Pi run.
type RunResult struct {
	Role     agent.Role
	Space    string
	Task     string
	Output   string // stdout (the final assistant text in print mode)
	Stderr   string // tail of stderr
	ExitCode int
	Elapsed  time.Duration
	Outcome  agent.Outcome
	// Sessions is the aggregate over every session in SessionDir, i.e. the
	// cumulative spend of this role for this task. Nil when parsing failed.
	Sessions *SessionStats
	Targets  agent.Targets
	Policy   agent.CleanupPolicy
}

// Cleanup applies the role's cleanup policy for this run's outcome. Call it
// only after the worktree's results have been consumed (merged, reviewed).
func (r *RunResult) Cleanup(c agent.Cleaner) error {
	return c.Apply(r.Policy, r.Outcome, r.Targets)
}

// ExitError reports a non-zero exit of Pi.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("pi exited with code %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

// RunWorker runs a worker to completion. When Common.Tracker is set the
// task and space budgets are enforced first and the session spend is recorded
// afterwards. The returned result is non-nil whenever Pi was started, even if
// the error is non-nil.
func RunWorker(ctx context.Context, o WorkerOptions) (*RunResult, error) {
	cmd, targets, err := BuildWorkerCommand(o)
	if err != nil {
		return nil, err
	}
	cfg, _ := o.config(agent.RoleWorker)
	return runOneShot(ctx, o.Common, cfg, agent.RoleWorker, o.Space, o.Task, cmd, targets)
}

// RunReviewer runs a reviewer and parses its verdict. The RunResult is
// returned alongside any parse error so callers can still account for cost.
func RunReviewer(ctx context.Context, o ReviewerOptions) (*agent.ReviewVerdict, *RunResult, error) {
	cmd, targets, err := BuildReviewerCommand(o)
	if err != nil {
		return nil, nil, err
	}
	cfg, _ := o.config(agent.RoleReviewer)
	res, err := runOneShot(ctx, o.Common, cfg, agent.RoleReviewer, o.Space, o.Task, cmd, targets)
	if err != nil {
		return nil, res, err
	}
	v, err := agent.ParseReviewVerdict(res.Output)
	if err != nil {
		res.Outcome = agent.OutcomeFailed
		return nil, res, err
	}
	return v, res, nil
}

func runOneShot(ctx context.Context, c Common, cfg agent.Config, role agent.Role, space, task string, cmd Command, targets agent.Targets) (*RunResult, error) {
	if c.Tracker != nil {
		if err := c.Tracker.Enforce(space, task); err != nil {
			return nil, err
		}
	}
	runCtx := ctx
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	res := &RunResult{Role: role, Space: space, Task: task, Targets: targets, Policy: cfg.Cleanup}
	out := &limitedBuffer{max: maxOutputBytes}
	errTail := &tailBuffer{max: stderrTailBytes}
	ec := cmd.Exec(runCtx)
	ec.Stdout, ec.Stderr = out, errTail

	start := time.Now()
	runErr := ec.Run()
	res.Elapsed = time.Since(start)
	res.Output = out.String()
	res.Stderr = errTail.String()

	var startFailed bool
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.Outcome = agent.OutcomeSucceeded
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		startFailed = ec.ProcessState == nil
	}
	if runErr != nil {
		res.Outcome = agent.OutcomeFailed
		if ctx.Err() != nil {
			res.Outcome = agent.OutcomeCancelled
		}
	}
	if startFailed {
		return nil, fmt.Errorf("pi: start %s: %w", cmd.Path, runErr)
	}

	var accountErr error
	if sess, err := ParseSessionDirSum(targets.SessionDir); err == nil {
		res.Sessions = sess
		if c.Tracker != nil {
			_, accountErr = c.Tracker.IngestDir(space, task, role, targets.SessionDir)
		}
	} else {
		accountErr = err
	}

	switch {
	case runErr == nil:
		return res, accountErr
	case res.Outcome == agent.OutcomeCancelled:
		return res, fmt.Errorf("pi: %s run cancelled: %w", role, context.Cause(ctx))
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("pi: %s run timed out after %s: %w", role, cfg.Timeout, runCtx.Err())
	case exitErr != nil:
		return res, &ExitError{Code: res.ExitCode, Stderr: res.Stderr}
	}
	return res, runErr
}

// ParseSessionDirSum parses all sessions in dir and returns their aggregate.
func ParseSessionDirSum(dir string) (*SessionStats, error) {
	if dir == "" {
		return nil, errors.New("pi: no session dir")
	}
	sessions, err := ParseSessionDir(dir)
	if err != nil {
		return nil, err
	}
	return SumSessions(sessions), nil
}

// ---------------------------------------------------------------- Buffers

// limitedBuffer keeps the first max bytes and discards the rest.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// ---------------------------------------------------------------- RPC process

// Event is one JSONL record read from a Pi RPC process's stdout.
type Event struct {
	Type string          // the record's "type" field
	Raw  json.RawMessage // the complete record
}

// Process is a running Pi RPC (headless) process.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event
	stderr *tailBuffer

	sendMu   sync.Mutex
	nextID   int
	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	waitErr  error
}

// StartProcess starts c with piped stdin/stdout for the RPC protocol.
// Events must be consumed (see Events) or the process will stall on stdout
// backpressure. Cancelling ctx terminates the process.
func StartProcess(ctx context.Context, c Command) (*Process, error) {
	cmd := c.Exec(ctx)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	p := &Process{
		cmd: cmd, stdin: stdin, events: make(chan Event, 64),
		stderr: &tailBuffer{max: stderrTailBytes},
		stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pi: start %s: %w", c.Path, err)
	}
	go p.readLoop(stdout)
	return p, nil
}

// readLoop splits stdout on LF only (never U+2028/2029) per the RPC framing
// rules, then reaps the process.
func (p *Process) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			ev := Event{Raw: json.RawMessage(line)}
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &head) == nil {
				ev.Type = head.Type
			}
			select {
			case p.events <- ev:
			case <-p.stopCh:
			}
		}
		if err != nil {
			break
		}
	}
	close(p.events)
	p.waitErr = p.cmd.Wait()
	close(p.done)
}

// PID returns the OS process id.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Events returns the channel of stdout records; it is closed when the process
// exits.
func (p *Process) Events() <-chan Event { return p.events }

// Stderr returns the tail of the process's stderr.
func (p *Process) Stderr() string { return p.stderr.String() }

// Done is closed once the process has exited and been reaped.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until exit and returns the exit error, if any.
func (p *Process) Wait() error {
	<-p.done
	return p.waitErr
}

// Send writes one RPC command as a JSONL record.
func (p *Process) Send(cmd map[string]any) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("pi: encode rpc command: %w", err)
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	select {
	case <-p.done:
		return errors.New("pi: process has exited")
	default:
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("pi: write rpc command: %w", err)
	}
	return nil
}

func (p *Process) newID() string {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	p.nextID++
	return fmt.Sprintf("fleet-%d", p.nextID)
}

// Prompt sends a prompt command. While the agent is streaming, use PromptSteer
// or PromptFollowUp instead.
func (p *Process) Prompt(message string) error {
	return p.Send(map[string]any{"id": p.newID(), "type": "prompt", "message": message})
}

// PromptSteer queues a message delivered before the next LLM call.
func (p *Process) PromptSteer(message string) error {
	return p.Send(map[string]any{"id": p.newID(), "type": "prompt", "message": message, "streamingBehavior": "steer"})
}

// PromptFollowUp queues a message delivered when the agent stops.
func (p *Process) PromptFollowUp(message string) error {
	return p.Send(map[string]any{"id": p.newID(), "type": "prompt", "message": message, "streamingBehavior": "followUp"})
}

// Abort aborts the current agent operation.
func (p *Process) Abort() error {
	return p.Send(map[string]any{"id": p.newID(), "type": "abort"})
}

// Stop shuts the process down: it closes stdin, sends SIGTERM to the process
// group, and after grace sends SIGKILL. It returns once the process is reaped.
// Stop is idempotent and safe to call concurrently.
func (p *Process) Stop(grace time.Duration) error {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		_ = p.stdin.Close()
		_ = signalGroup(p.cmd, syscall.SIGTERM)
	})
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = signalGroup(p.cmd, syscall.SIGKILL)
		<-p.done
	}
	return nil
}
