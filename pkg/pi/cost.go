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

	"fleet/pkg/budget"
)

// Types re-exported from pkg/budget.
type (
	Cost         = budget.Cost
	Usage        = budget.Usage
	ModelUsage   = budget.ModelUsage
	SessionStats = budget.SessionStats
)

const IdleGap = budget.IdleGap

var SumSessions = budget.SumSessions


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
			if perr := consume(st, line, seen, &curProvider, &curModel, &prev); perr != nil {
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

func consume(st *SessionStats, line []byte, seen map[string]struct{}, curProv, curModel *string, prev *time.Time) error {
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
			addUsage(st, prov, model, e.Message.Usage)
		case "toolResult": // nested model work, when present
			addUsage(st, *curProv, *curModel, e.Message.Usage)
		}
	case "usage":
		prov, model := e.Provider, e.Model
		if model == "" {
			prov, model = *curProv, *curModel
		}
		addUsage(st, prov, model, e.Usage)
	case "compaction", "branch_summary":
		addUsage(st, *curProv, *curModel, e.Usage)
	}
	return nil
}

func addUsage(st *SessionStats, provider, model string, ru *rawUsage) {
	if ru == nil {
		return
	}
	u := ru.toUsage()
	if u.IsZero() {
		return
	}
	key := budget.ModelKey(provider, model)
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

