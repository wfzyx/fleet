package driver

import (
	"context"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/budget"
)

// WorkerRequest describes a one-shot worker invocation.
type WorkerRequest struct {
	Space      string
	Task       string
	Title      string
	Brief      string
	Worktree   string
	Branch     string
	BaseBranch string
	Acceptance []string
	Timeout    time.Duration
	Env        []string
}

// WorkerResult is the outcome of a worker run.
type WorkerResult struct {
	Output   string
	Stderr   string
	ExitCode int
	Elapsed  time.Duration
	Outcome  agent.Outcome
	Sessions *budget.SessionStats
}

// ReviewerRequest describes a one-shot reviewer invocation.
type ReviewerRequest struct {
	Space    string
	Task     string
	Title    string
	Criteria []string
	Diff     string
	Worktree string
	Timeout  time.Duration
	Env      []string
}

// ReviewerResult is the outcome of a reviewer run.
type ReviewerResult struct {
	Verdict  *agent.ReviewVerdict
	Output   string
	Stderr   string
	ExitCode int
	Elapsed  time.Duration
	Outcome  agent.Outcome
	Sessions *budget.SessionStats
}

// LeadRequest describes how to launch a persistent lead.
type LeadRequest struct {
	Space   string
	Summary string
	Dir     string
	Env     []string
}

// LeadSession represents a running conversational lead process.
type LeadSession interface {
	// Prompt sends a message to the lead and blocks until the turn completes,
	// returning the assistant's reply.
	Prompt(ctx context.Context, message string) (string, error)
	// Done signals when the lead process exits unexpectedly.
	Done() <-chan struct{}
	// Abort interrupts an in-progress prompt turn.
	Abort() error
	// Stop terminates the lead process gracefully within the grace period.
	Stop(grace time.Duration) error
}

// SessionParser extracts session statistics from an agent's session directory.
// Drivers can implement this to enable session log ingestion into Fleet's budget ledger.
type SessionParser interface {
	ParseSessions(dir string) ([]*budget.SessionStats, error)
}

// Driver is the interface implemented by coding agent engines (Pi, Claude Code, Aider, etc.).
type Driver interface {
	Name() string
	RunWorker(ctx context.Context, req WorkerRequest) (*WorkerResult, error)
	RunReviewer(ctx context.Context, req ReviewerRequest) (*ReviewerResult, error)
	StartLead(ctx context.Context, req LeadRequest) (LeadSession, error)
}
