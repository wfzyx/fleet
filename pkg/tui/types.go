package tui

import (
	"context"
	"errors"

	"fleet/pkg/schema"
)

// LeadStatus is the live state of a space's lead agent.
type LeadStatus int

const (
	LeadOffline LeadStatus = iota
	LeadIdle
	LeadWorking
)

func (l LeadStatus) String() string {
	switch l {
	case LeadIdle:
		return "idle"
	case LeadWorking:
		return "working"
	}
	return "offline"
}

// SpaceInfo is the sidebar view-model of one registered space.
type SpaceInfo struct {
	ID          string
	Name        string
	SpendUSD    float64
	BudgetUSD   float64 // <= 0 means unlimited
	Lead        LeadStatus
	ActiveTasks int // queued + working + review
}

// TaskDetail is the slow-changing data shown by the inspector.
type TaskDetail struct {
	Diff    string                // unified diff of the worktree
	Logs    []string              // recent worker turn log lines, oldest first
	Verdict *schema.ReviewVerdict // latest reviewer verdict, if any
}

// Backend connects the TUI to the fleet. Every method is called from a
// tea.Cmd goroutine, never from Update, and must honour ctx cancellation.
// Any method may be left unimplemented by embedding NopBackend.
type Backend interface {
	Spaces(ctx context.Context) ([]SpaceInfo, error)
	Tasks(ctx context.Context, spaceID string) ([]schema.Task, error)
	Detail(ctx context.Context, task schema.Task) (TaskDetail, error)
	// Chat sends one message to the space's lead and returns its reply.
	Chat(ctx context.Context, spaceID, text string) (string, error)
	// CreateTask registers a queued task.
	CreateTask(ctx context.Context, spaceID, title, brief string) (schema.Task, error)
	// Delegate hands a queued task to a worker (queued -> working).
	Delegate(ctx context.Context, task schema.Task) error
	// RequestReview starts a review (working -> review).
	RequestReview(ctx context.Context, task schema.Task) error
	// Approve approves and merges a reviewed task (review -> done).
	Approve(ctx context.Context, task schema.Task) error
}

// Messages. Anything outside the package (for example an event-bus reader)
// may inject EventMsg, SpacesMsg, TasksMsg or DetailMsg with Program.Send.
type (
	// SpacesMsg delivers a fresh space list.
	SpacesMsg struct {
		Spaces []SpaceInfo
		Err    error
	}
	// TasksMsg delivers the tasks of one space.
	TasksMsg struct {
		SpaceID string
		Tasks   []schema.Task
		Err     error
	}
	// DetailMsg delivers inspector data for one task.
	DetailMsg struct {
		TaskID string
		Detail TaskDetail
		Err    error
	}
	// ChatReplyMsg is the lead's answer to a chat message.
	ChatReplyMsg struct {
		SpaceID string
		Reply   string
		Err     error
	}
	// ActionMsg reports the result of a user-triggered action.
	ActionMsg struct {
		Label  string
		TaskID string // task to highlight after the refresh, if any
		Err    error
	}
	// EventMsg injects a live agent event.
	EventMsg struct{ Event schema.AgentEvent }
)

// ErrNoBackend is returned by NopBackend and used when no Backend is set.
var ErrNoBackend = errors.New("no backend connected")

// NopBackend implements Backend with empty data and ErrNoBackend for actions.
// Embed it to implement only part of the interface.
type NopBackend struct{}

func (NopBackend) Spaces(context.Context) ([]SpaceInfo, error) { return nil, nil }
func (NopBackend) Tasks(context.Context, string) ([]schema.Task, error) {
	return nil, nil
}
func (NopBackend) Detail(context.Context, schema.Task) (TaskDetail, error) {
	return TaskDetail{}, nil
}
func (NopBackend) Chat(context.Context, string, string) (string, error) { return "", ErrNoBackend }
func (NopBackend) CreateTask(context.Context, string, string, string) (schema.Task, error) {
	return schema.Task{}, ErrNoBackend
}
func (NopBackend) Delegate(context.Context, schema.Task) error      { return ErrNoBackend }
func (NopBackend) RequestReview(context.Context, schema.Task) error { return ErrNoBackend }
func (NopBackend) Approve(context.Context, schema.Task) error       { return ErrNoBackend }
