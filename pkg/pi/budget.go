package pi

import (
	"fleet/pkg/agent"
	"fleet/pkg/budget"
)

// ErrBudgetExceeded is returned (wrapped) when spending has reached a limit.
var ErrBudgetExceeded = budget.ErrBudgetExceeded

// DefaultWarnRatio is the fraction of a limit at which status becomes Warning.
const DefaultWarnRatio = budget.DefaultWarnRatio

type (
	Budgets      = budget.Budgets
	Scope        = budget.Scope
	BudgetState  = budget.BudgetState
	BudgetStatus = budget.BudgetStatus
	Entry        = budget.Entry
	Tracker      = budget.Tracker
)

const (
	ScopeTask       = budget.ScopeTask
	ScopeSpace      = budget.ScopeSpace
	BudgetUnlimited = budget.BudgetUnlimited
	BudgetOK        = budget.BudgetOK
	BudgetWarning   = budget.BudgetWarning
	BudgetExceeded  = budget.BudgetExceeded
)

var (
	NewTracker = budget.NewTracker
	Classify   = budget.Classify
	LedgerPath = budget.LedgerPath
)

// IngestDir parses every session below dir and records it in t. It returns the
// aggregate of the parsed sessions. A missing dir records nothing.
func IngestDir(t *budget.Tracker, space, task string, role agent.Role, dir string) (*budget.SessionStats, error) {
	sessions, err := ParseSessionDir(dir)
	if err != nil {
		return nil, err
	}
	return t.IngestSessions(space, task, role, sessions)
}
