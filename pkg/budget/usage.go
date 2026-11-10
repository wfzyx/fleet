package budget

import (
	"sort"
	"time"
)

// IdleGap is the largest gap between two session entries that still counts as
// active time in SessionStats.ActiveDuration.
const IdleGap = 10 * time.Minute

// Cost is a dollar cost split by token class.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Total      float64 `json:"total"`
}

// Usage is an aggregate of token counts and cost.
type Usage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheWrite  int64 `json:"cache_write"`
	Reasoning   int64 `json:"reasoning,omitempty"` // already included in Output
	TotalTokens int64 `json:"total_tokens"`
	Cost        Cost  `json:"cost"`
}

// Add accumulates o into u.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
	u.Reasoning += o.Reasoning
	u.TotalTokens += o.TotalTokens
	u.Cost.Input += o.Cost.Input
	u.Cost.Output += o.Cost.Output
	u.Cost.CacheRead += o.Cost.CacheRead
	u.Cost.CacheWrite += o.Cost.CacheWrite
	u.Cost.Total += o.Cost.Total
}

// IsZero reports whether no tokens or dollars are recorded.
func (u Usage) IsZero() bool { return u == Usage{} }

// ModelUsage is the usage attributed to one provider/model pair.
type ModelUsage struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	Usage
}
// Key returns "provider/model", or just "model" when provider is empty.
func (m ModelUsage) Key() string { return ModelKey(m.Provider, m.Model) }

// ModelKey formats a provider and model as "provider/model", or just model.
func ModelKey(provider, model string) string {
	switch {
	case model == "":
		return "unknown"
	case provider == "":
		return model
	}
	return provider + "/" + model
}

// SessionStats is the usage, cost and timing of one session file (or the
// aggregate of several sessions for the same role/task).
type SessionStats struct {
	Path string `json:"path,omitempty"`
	ID   string `json:"id,omitempty"`
	CWD  string `json:"cwd,omitempty"`
	Name string `json:"name,omitempty"`

	Start time.Time `json:"start,omitempty"`
	End   time.Time `json:"end,omitempty"`
	// Duration is wall-clock time from the first to the last entry.
	Duration time.Duration `json:"duration"`
	// ActiveDuration is Duration without gaps longer than IdleGap, which is
	// the meaningful figure for long-lived lead sessions.
	ActiveDuration time.Duration `json:"active_duration"`

	Total Usage `json:"total"`
	// ByModel is keyed by "provider/model" and sorted via Models().
	ByModel map[string]*ModelUsage `json:"by_model"`

	// Messages counts user and assistant messages.
	Messages int `json:"messages"`
	// Malformed counts lines that were not valid JSON (e.g. a torn last line
	// of a session still being written).
	Malformed int `json:"malformed,omitempty"`
}

// Models returns the ByModel entries sorted by descending total cost.
func (s *SessionStats) Models() []ModelUsage {
	if s == nil || len(s.ByModel) == 0 {
		return nil
	}
	out := make([]ModelUsage, 0, len(s.ByModel))
	for _, m := range s.ByModel {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost.Total != out[j].Cost.Total {
			return out[i].Cost.Total > out[j].Cost.Total
		}
		return out[i].Key() < out[j].Key()
	})
	return out
}

// PrimaryModel returns the model that incurred the largest cost.
func (s *SessionStats) PrimaryModel() string {
	m := s.Models()
	if len(m) == 0 {
		return ""
	}
	return m[0].Key()
}

// SumSessions aggregates multiple SessionStats into one.
func SumSessions(sessions []*SessionStats) *SessionStats {
	if len(sessions) == 0 {
		return &SessionStats{ByModel: map[string]*ModelUsage{}}
	}
	agg := &SessionStats{ByModel: map[string]*ModelUsage{}}
	for _, s := range sessions {
		if s == nil {
			continue
		}
		if agg.Path == "" {
			agg.Path = s.Path
		}
		if agg.ID == "" {
			agg.ID = s.ID
		}
		agg.Total.Add(s.Total)
		agg.Duration += s.Duration
		agg.ActiveDuration += s.ActiveDuration
		agg.Messages += s.Messages
		agg.Malformed += s.Malformed
		if agg.Start.IsZero() || (!s.Start.IsZero() && s.Start.Before(agg.Start)) {
			agg.Start = s.Start
		}
		if s.End.After(agg.End) {
			agg.End = s.End
		}
		for k, m := range s.ByModel {
			cur := agg.ByModel[k]
			if cur == nil {
				cur = &ModelUsage{Provider: m.Provider, Model: m.Model}
				agg.ByModel[k] = cur
			}
			cur.Requests += m.Requests
			cur.Usage.Add(m.Usage)
		}
	}
	return agg
}
