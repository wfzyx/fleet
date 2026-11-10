// Package pi parses Pi session files for usage and cost, tracks spending
// against budgets, and launches Pi processes for the fleet roles.
package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// Requests counts usage-bearing entries (assistant messages, usage
	// entries, compaction and branch summaries).
	Requests int `json:"requests"`
	Usage
}

// Key returns "provider/model", or just the model when no provider is known.
func (m ModelUsage) Key() string { return modelKey(m.Provider, m.Model) }

func modelKey(provider, model string) string {
	switch {
	case model == "":
		return "unknown"
	case provider == "":
		return model
	}
	return provider + "/" + model
}

// SessionStats is everything extracted from one session file.
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

// Models returns the per-model usage sorted by descending cost, then key.
func (s *SessionStats) Models() []ModelUsage {
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

// PrimaryModel returns the model key with the highest cost ("" if none).
func (s *SessionStats) PrimaryModel() string {
	if m := s.Models(); len(m) > 0 {
		return m[0].Key()
	}
	return ""
}

// rawUsage mirrors Pi's Usage object.
type rawUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	Reasoning   int64 `json:"reasoning"`
	TotalTokens int64 `json:"totalTokens"`
	Cost        struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Total      float64 `json:"total"`
	} `json:"cost"`
}

func (r *rawUsage) toUsage() Usage {
	u := Usage{
		Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite,
		Reasoning: r.Reasoning, TotalTokens: r.TotalTokens,
		Cost: Cost{
			Input: r.Cost.Input, Output: r.Cost.Output,
			CacheRead: r.Cost.CacheRead, CacheWrite: r.Cost.CacheWrite, Total: r.Cost.Total,
		},
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.Input + u.Output + u.CacheRead + u.CacheWrite
	}
	if u.Cost.Total == 0 {
		u.Cost.Total = u.Cost.Input + u.Cost.Output + u.Cost.CacheRead + u.Cost.CacheWrite
	}
	return u
}

// rawEntry is the union of the session entry fields this package reads.
type rawEntry struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`

	// session header
	CWD string `json:"cwd"`

	// model_change
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`

	// session_info
	Name string `json:"name"`

	// usage, compaction, branch_summary
	Model string    `json:"model"`
	Usage *rawUsage `json:"usage"`

	Message *struct {
		Role     string    `json:"role"`
		Provider string    `json:"provider"`
		Model    string    `json:"model"`
		Usage    *rawUsage `json:"usage"`
	} `json:"message"`
}

// ParseSessionFile parses the Pi session JSONL file at path.
func ParseSessionFile(path string) (*SessionStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pi: open session: %w", err)
	}
	defer f.Close()
	st, err := ParseSession(f)
	if err != nil {
		return nil, fmt.Errorf("pi: parse %s: %w", path, err)
	}
	st.Path = path
	return st, nil
}

// ParseSession parses a Pi session JSONL stream. Usage is summed over every
// entry on every branch (spend is spent regardless of which branch is active),
// deduplicated by entry id. Lines that are not valid JSON are counted in
// Malformed and skipped.
func ParseSession(r io.Reader) (*SessionStats, error) {
	st := &SessionStats{ByModel: map[string]*ModelUsage{}}
	br := bufio.NewReaderSize(r, 1<<20) // lines can hold whole system prompts
	seen := map[string]struct{}{}
	var curProvider, curModel string
	var prev time.Time

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if perr := st.consume(line, seen, &curProvider, &curModel, &prev); perr != nil {
				return nil, perr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if !st.Start.IsZero() && st.End.After(st.Start) {
		st.Duration = st.End.Sub(st.Start)
	}
	return st, nil
}

func (st *SessionStats) consume(line []byte, seen map[string]struct{}, curProv, curModel *string, prev *time.Time) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	var e rawEntry
	if err := json.Unmarshal(line, &e); err != nil {
		st.Malformed++
		return nil
	}

	if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
		if st.Start.IsZero() || ts.Before(st.Start) {
			st.Start = ts
		}
		if ts.After(st.End) {
			st.End = ts
		}
		if !prev.IsZero() {
			if gap := ts.Sub(*prev); gap > 0 && gap <= IdleGap {
				st.ActiveDuration += gap
			}
		}
		if ts.After(*prev) {
			*prev = ts
		}
	}

	if e.ID != "" && e.Type != "session" {
		if _, dup := seen[e.ID]; dup {
			return nil
		}
		seen[e.ID] = struct{}{}
	}

	switch e.Type {
	case "session":
		st.ID, st.CWD = e.ID, e.CWD
	case "session_info":
		st.Name = e.Name
	case "model_change":
		*curProv, *curModel = e.Provider, e.ModelID
	case "message":
		if e.Message == nil {
			return nil
		}
		switch e.Message.Role {
		case "user":
			st.Messages++
		case "assistant":
			st.Messages++
			prov, model := e.Message.Provider, e.Message.Model
			if model == "" {
				prov, model = *curProv, *curModel
			}
			st.addUsage(prov, model, e.Message.Usage)
		case "toolResult": // nested model work, when present
			st.addUsage(*curProv, *curModel, e.Message.Usage)
		}
	case "usage":
		prov, model := e.Provider, e.Model
		if model == "" {
			prov, model = *curProv, *curModel
		}
		st.addUsage(prov, model, e.Usage)
	case "compaction", "branch_summary":
		st.addUsage(*curProv, *curModel, e.Usage)
	}
	return nil
}

func (st *SessionStats) addUsage(provider, model string, ru *rawUsage) {
	if ru == nil {
		return
	}
	u := ru.toUsage()
	if u.IsZero() {
		return
	}
	key := modelKey(provider, model)
	mu := st.ByModel[key]
	if mu == nil {
		mu = &ModelUsage{Provider: provider, Model: model}
		st.ByModel[key] = mu
	}
	mu.Requests++
	mu.Usage.Add(u)
	st.Total.Add(u)
}

// DefaultSessionsDir returns Pi's session root: $PI_CODING_AGENT_SESSION_DIR,
// else $PI_CODING_AGENT_DIR/sessions, else ~/.pi/agent/sessions.
func DefaultSessionsDir() (string, error) {
	if v := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); v != "" {
		return v, nil
	}
	if v := os.Getenv("PI_CODING_AGENT_DIR"); v != "" {
		return filepath.Join(v, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pi: resolve sessions dir: %w", err)
	}
	return filepath.Join(home, ".pi", "agent", "sessions"), nil
}

// ParseSessionDir parses every *.jsonl file below dir (recursively, including
// archive folders) and returns the stats sorted by start time then path.
// Unreadable files abort the walk; an absent dir yields no sessions and no error.
func ParseSessionDir(dir string) ([]*SessionStats, error) {
	var out []*SessionStats
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		st, err := ParseSessionFile(path)
		if err != nil {
			return err
		}
		out = append(out, st)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// SumSessions aggregates usage, per-model usage and durations over sessions.
func SumSessions(sessions []*SessionStats) *SessionStats {
	agg := &SessionStats{ByModel: map[string]*ModelUsage{}}
	for _, s := range sessions {
		agg.Total.Add(s.Total)
		agg.Duration += s.Duration
		agg.ActiveDuration += s.ActiveDuration
		agg.Messages += s.Messages
		agg.Malformed += s.Malformed
		for k, m := range s.ByModel {
			a := agg.ByModel[k]
			if a == nil {
				a = &ModelUsage{Provider: m.Provider, Model: m.Model}
				agg.ByModel[k] = a
			}
			a.Requests += m.Requests
			a.Usage.Add(m.Usage)
		}
		if !s.Start.IsZero() && (agg.Start.IsZero() || s.Start.Before(agg.Start)) {
			agg.Start = s.Start
		}
		if s.End.After(agg.End) {
			agg.End = s.End
		}
	}
	return agg
}
