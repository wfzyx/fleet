package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EventType identifies an AgentEvent.
type EventType string

const (
	EventTaskStarted         EventType = "task_started"
	EventTaskProgress        EventType = "task_progress"
	EventTaskReviewRequested EventType = "task_review_requested"
	EventReviewVerdict       EventType = "review_verdict"
	EventTaskCompleted       EventType = "task_completed"
)

// Valid reports whether e is a known event type.
func (e EventType) Valid() bool {
	switch e {
	case EventTaskStarted, EventTaskProgress, EventTaskReviewRequested, EventReviewVerdict, EventTaskCompleted:
		return true
	}
	return false
}

// UnmarshalJSON rejects unknown event types.
func (e *EventType) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if !EventType(v).Valid() {
		return fmt.Errorf("invalid event type %q", v)
	}
	*e = EventType(v)
	return nil
}

// Decision is a reviewer's decision.
type Decision string

const (
	DecisionApproved         Decision = "approved"
	DecisionChangesRequested Decision = "changes_requested"
)

// Valid reports whether d is a known decision.
func (d Decision) Valid() bool {
	return d == DecisionApproved || d == DecisionChangesRequested
}

// UnmarshalJSON rejects unknown decisions.
func (d *Decision) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if !Decision(v).Valid() {
		return fmt.Errorf("invalid verdict %q", v)
	}
	*d = Decision(v)
	return nil
}

// ChecklistItem is one reviewed criterion.
type ChecklistItem struct {
	Item string `json:"item"`
	Met  bool   `json:"met"`
	Note string `json:"note,omitempty"`
}

// ReviewVerdict is the outcome of a review.
type ReviewVerdict struct {
	Verdict   Decision        `json:"verdict"`
	Checklist []ChecklistItem `json:"checklist"`
	Notes     string          `json:"notes,omitempty"`
}

// Validate checks the verdict. An approval requires every checklist item met;
// a changes_requested verdict requires notes or at least one unmet item.
func (v ReviewVerdict) Validate() error {
	if !v.Verdict.Valid() {
		return fmt.Errorf("verdict: invalid decision %q", v.Verdict)
	}
	unmet := 0
	for i, c := range v.Checklist {
		if c.Item == "" {
			return fmt.Errorf("verdict: checklist[%d] has empty item", i)
		}
		if !c.Met {
			unmet++
		}
	}
	switch v.Verdict {
	case DecisionApproved:
		if unmet > 0 {
			return errors.New("verdict: approved with unmet checklist items")
		}
	case DecisionChangesRequested:
		if unmet == 0 && v.Notes == "" {
			return errors.New("verdict: changes_requested needs notes or an unmet checklist item")
		}
	}
	return nil
}

// AgentEvent is a message emitted by an agent about a task.
type AgentEvent struct {
	Type      EventType      `json:"type"`
	TaskID    string         `json:"task_id"`
	SpaceID   string         `json:"space_id"`
	Role      AgentRole      `json:"role"`
	Timestamp time.Time      `json:"timestamp"`
	Message   string         `json:"message,omitempty"`
	PRUrl     string         `json:"pr_url,omitempty"`    // task_review_requested, task_completed
	SpendUSD  *float64       `json:"spend_usd,omitempty"` // optional incremental spend
	Verdict   *ReviewVerdict `json:"verdict,omitempty"`   // review_verdict only
}

// Validate checks common and per-type invariants.
func (e AgentEvent) Validate() error {
	switch {
	case !e.Type.Valid():
		return fmt.Errorf("event: invalid type %q", e.Type)
	case e.TaskID == "":
		return errors.New("event: task_id is required")
	case e.SpaceID == "":
		return errors.New("event: space_id is required")
	case !e.Role.Valid():
		return fmt.Errorf("event: invalid role %q", e.Role)
	case e.Timestamp.IsZero():
		return errors.New("event: timestamp is required")
	case e.SpendUSD != nil && *e.SpendUSD < 0:
		return errors.New("event: spend_usd must be non-negative")
	}
	if e.Type == EventReviewVerdict {
		if e.Verdict == nil {
			return errors.New("event: review_verdict requires verdict")
		}
		if e.Role != RoleReviewer {
			return errors.New("event: review_verdict must come from a reviewer")
		}
		return e.Verdict.Validate()
	}
	if e.Verdict != nil {
		return fmt.Errorf("event: verdict not allowed on %s", e.Type)
	}
	switch e.Type {
	case EventTaskProgress:
		if e.Message == "" {
			return errors.New("event: task_progress requires message")
		}
	case EventTaskReviewRequested:
		if e.Role != RoleWorker {
			return errors.New("event: task_review_requested must come from a worker")
		}
	}
	if e.PRUrl != "" && e.Type != EventTaskReviewRequested && e.Type != EventTaskCompleted {
		return fmt.Errorf("event: pr_url not allowed on %s", e.Type)
	}
	return nil
}

// MarshalEvent validates then encodes an event.
func MarshalEvent(e AgentEvent) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// UnmarshalEvent strictly decodes and validates an event.
func UnmarshalEvent(data []byte) (AgentEvent, error) {
	var e AgentEvent
	if err := strictDecode(data, &e); err != nil {
		return AgentEvent{}, fmt.Errorf("decode event: %w", err)
	}
	if err := e.Validate(); err != nil {
		return AgentEvent{}, err
	}
	return e, nil
}
