package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/budget"
	"fleet/pkg/driver"
)

// DriverOptions configures the Pi driver.
type DriverOptions struct {
	Binary     string
	Env        []string
	Extensions []string
	Tracker    *budget.Tracker
}

// Driver implements driver.Driver and driver.SessionParser using the Pi coding agent.
type Driver struct {
	opts DriverOptions
}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.SessionParser = (*Driver)(nil)
)

// NewDriver returns a new Pi driver.
func NewDriver(opts DriverOptions) *Driver {
	return &Driver{opts: opts}
}

func (d *Driver) Name() string { return "pi" }

func (d *Driver) common(role agent.Role) Common {
	return Common{
		Binary:     d.opts.Binary,
		Env:        d.opts.Env,
		Extensions: d.opts.Extensions,
		Tracker:    d.opts.Tracker,
	}
}

// ParseSessions implements driver.SessionParser.
func (d *Driver) ParseSessions(dir string) ([]*budget.SessionStats, error) {
	return ParseSessionDir(dir)
}

func (d *Driver) RunWorker(ctx context.Context, req driver.WorkerRequest) (*driver.WorkerResult, error) {
	opts := WorkerOptions{
		Common:     d.common(agent.RoleWorker),
		Space:      req.Space,
		Task:       req.Task,
		Title:      req.Title,
		Brief:      req.Brief,
		Worktree:   req.Worktree,
		Branch:     req.Branch,
		BaseBranch: req.BaseBranch,
		Acceptance: req.Acceptance,
	}
	res, err := RunWorker(ctx, opts)
	if err != nil && res == nil {
		return nil, err
	}
	out := &driver.WorkerResult{
		Output:   res.Output,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		Elapsed:  res.Elapsed,
		Outcome:  res.Outcome,
		Sessions: res.Sessions,
	}
	return out, err
}

func (d *Driver) RunReviewer(ctx context.Context, req driver.ReviewerRequest) (*driver.ReviewerResult, error) {
	opts := ReviewerOptions{
		Common:   d.common(agent.RoleReviewer),
		Space:    req.Space,
		Task:     req.Task,
		Title:    req.Title,
		Criteria: req.Criteria,
		Diff:     req.Diff,
		Worktree: req.Worktree,
	}
	verdict, res, err := RunReviewer(ctx, opts)
	if err != nil && res == nil {
		return nil, err
	}
	out := &driver.ReviewerResult{
		Verdict:  verdict,
		Output:   res.Output,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		Elapsed:  res.Elapsed,
		Outcome:  res.Outcome,
		Sessions: res.Sessions,
	}
	return out, err
}

func (d *Driver) StartLead(ctx context.Context, req driver.LeadRequest) (driver.LeadSession, error) {
	opts := LeadOptions{
		Common:  d.common(agent.RoleLead),
		Space:   req.Space,
		Summary: req.Summary,
		Dir:     req.Dir,
	}
	proc, err := StartLead(ctx, opts)
	if err != nil {
		return nil, err
	}
	// The intro turn is not part of the conversation: wait it out within 2 minutes.
	intro, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := AwaitSettled(intro, proc); err != nil {
		_ = proc.Abort()
		_ = proc.Stop(5 * time.Second)
		return nil, fmt.Errorf("pi: lead intro: %w", err)
	}
	return &leadSession{proc: proc}, nil
}

type leadSession struct {
	proc *Process
}

func (s *leadSession) Prompt(ctx context.Context, message string) (string, error) {
	if err := s.proc.Prompt(message); err != nil {
		return "", err
	}
	return AwaitSettled(ctx, s.proc)
}

func (s *leadSession) Done() <-chan struct{} { return s.proc.Done() }
func (s *leadSession) Abort() error          { return s.proc.Abort() }
func (s *leadSession) Stop(grace time.Duration) error { return s.proc.Stop(grace) }

// AwaitSettled consumes proc's events until Pi reports it is settled and
// returns the text of the last assistant message of that turn.
func AwaitSettled(ctx context.Context, proc *Process) (string, error) {
	var last, failure string
	events, done := proc.Events(), proc.Done()
	var grace <-chan time.Time
	exited := errors.New("lead process exited")
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			done = nil
			if events == nil {
				return "", exited
			}
			grace = time.After(100 * time.Millisecond)
		case <-grace:
			return "", exited
		case ev, ok := <-events:
			if !ok {
				events = nil
				if done == nil {
					return "", exited
				}
				continue
			}
			switch ev.Type {
			case "message_end":
				if text, errText, ok := assistantMessage(ev.Raw); ok {
					if text != "" {
						last = text
					}
					if errText != "" {
						failure = errText
					}
				}
			case "response":
				var r struct {
					Success bool   `json:"success"`
					Error   string `json:"error"`
				}
				if json.Unmarshal(ev.Raw, &r) == nil && !r.Success && r.Error != "" {
					return "", errors.New("lead: " + r.Error)
				}
			case "agent_settled":
				if last == "" && failure != "" {
					return "", errors.New("lead: " + failure)
				}
				return last, nil
			}
		}
	}
}

func assistantMessage(raw json.RawMessage) (text, errText string, ok bool) {
	var rec struct {
		Message struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &rec) != nil || rec.Message.Role != "assistant" {
		return "", "", false
	}
	var str string
	if json.Unmarshal(rec.Message.Content, &str) == nil {
		text = str
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rec.Message.Content, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.Join(parts, "\n\n")
		}
	}
	if rec.Message.StopReason == "error" {
		errText = rec.Message.ErrorMessage
		if errText == "" {
			errText = "error stop reason"
		}
	}
	return text, errText, true
}
