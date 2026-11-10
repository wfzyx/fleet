// Package schema defines the strict JSON wire types shared by fleet agents.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// Status is a task lifecycle state.
type Status string

const (
	StatusQueued  Status = "queued"
	StatusWorking Status = "working"
	StatusReview  Status = "review"
	StatusDone    Status = "done"
	StatusClosed  Status = "closed"
)

// Statuses lists all valid statuses.
func Statuses() []Status {
	return []Status{StatusQueued, StatusWorking, StatusReview, StatusDone, StatusClosed}
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusQueued, StatusWorking, StatusReview, StatusDone, StatusClosed:
		return true
	}
	return false
}

// Terminal reports whether no further transitions are allowed.
func (s Status) Terminal() bool { return s == StatusDone || s == StatusClosed }

var transitions = map[Status][]Status{
	StatusQueued:  {StatusWorking, StatusClosed},
	StatusWorking: {StatusReview, StatusQueued, StatusClosed},
	StatusReview:  {StatusDone, StatusWorking, StatusClosed},
}

// CanTransition reports whether moving from s to next is allowed.
func (s Status) CanTransition(next Status) bool {
	for _, n := range transitions[s] {
		if n == next {
			return true
		}
	}
	return false
}

// UnmarshalJSON rejects unknown status values.
func (s *Status) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if !Status(v).Valid() {
		return fmt.Errorf("invalid status %q", v)
	}
	*s = Status(v)
	return nil
}

// AgentRole is the role of an agent in the fleet.
type AgentRole string

const (
	RoleLead     AgentRole = "lead"
	RoleWorker   AgentRole = "worker"
	RoleReviewer AgentRole = "reviewer"
)

// Valid reports whether r is a known role.
func (r AgentRole) Valid() bool {
	switch r {
	case RoleLead, RoleWorker, RoleReviewer:
		return true
	}
	return false
}

// UnmarshalJSON rejects unknown role values.
func (r *AgentRole) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if !AgentRole(v).Valid() {
		return fmt.Errorf("invalid agent role %q", v)
	}
	*r = AgentRole(v)
	return nil
}

// Task is a unit of work delegated to a worker.
type Task struct {
	ID                 string   `json:"id"`
	SpaceID            string   `json:"space_id"`
	ProjectID          string   `json:"project_id,omitempty"`
	Title              string   `json:"title"`
	Brief              string   `json:"brief"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Status             Status   `json:"status"`
	AssignedWorker     string   `json:"assigned_worker,omitempty"`
	Branch             string   `json:"branch,omitempty"`
	WorktreePath       string   `json:"worktree_path,omitempty"`
	PRUrl              string   `json:"pr_url,omitempty"`
	JiraKey            string   `json:"jira_key,omitempty"`
	SpendUSD           float64  `json:"spend_usd"`
}

// Validate checks required fields and invariants.
func (t Task) Validate() error {
	switch {
	case t.ID == "":
		return errors.New("task: id is required")
	case t.SpaceID == "":
		return errors.New("task: space_id is required")
	case t.Title == "":
		return errors.New("task: title is required")
	case !t.Status.Valid():
		return fmt.Errorf("task: invalid status %q", t.Status)
	case t.SpendUSD < 0 || math.IsNaN(t.SpendUSD) || math.IsInf(t.SpendUSD, 0):
		return errors.New("task: spend_usd must be finite and non-negative")
	}
	for i, c := range t.AcceptanceCriteria {
		if c == "" {
			return fmt.Errorf("task: acceptance_criteria[%d] is empty", i)
		}
	}
	if t.Status != StatusQueued && t.Status != StatusClosed && t.AssignedWorker == "" {
		return fmt.Errorf("task: assigned_worker is required in status %q", t.Status)
	}
	return nil
}

// MarshalTask validates then encodes a task.
func MarshalTask(t Task) ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if t.AcceptanceCriteria == nil {
		t.AcceptanceCriteria = []string{}
	}
	return json.Marshal(t)
}

// UnmarshalTask strictly decodes and validates a task.
func UnmarshalTask(data []byte) (Task, error) {
	var t Task
	if err := strictDecode(data, &t); err != nil {
		return Task{}, fmt.Errorf("decode task: %w", err)
	}
	if err := t.Validate(); err != nil {
		return Task{}, err
	}
	return t, nil
}

// strictDecode rejects unknown fields, null documents and trailing data.
func strictDecode(data []byte, v any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null document")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after JSON value")
	}
	return nil
}
