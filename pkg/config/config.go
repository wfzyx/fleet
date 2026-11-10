package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// CurrentVersion is the config schema version.
const CurrentVersion = 1

// Default models per tier.
const (
	DefaultLeadModel     = "anthropic/claude-opus-4-5"
	DefaultWorkerModel   = "google/gemini-flash"
	DefaultReviewerModel = "anthropic/claude-opus-4-5"
)

// Project is a repository belonging to a Space.
type Project struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"` // absolute path of the user's checkout
	BaseBranch string `json:"base_branch,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

// Space groups projects managed together.
type Space struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Projects []Project `json:"projects"`
	// MaxBudgetUSD caps the total spend of the space; 0 means unlimited.
	MaxBudgetUSD float64 `json:"max_budget_usd,omitempty"`
	// TaskBudgetUSD caps the spend of each task in the space; 0 means unlimited.
	TaskBudgetUSD float64 `json:"task_budget_usd,omitempty"`
}

// Budget tracks spend against a limit. MaxBudgetUSD == 0 means unlimited.
type Budget struct {
	MaxBudgetUSD    float64 `json:"max_budget_usd"`
	CurrentSpendUSD float64 `json:"current_spend_usd"`
}

// Models names the default model for each agent tier.
type Models struct {
	Lead     string `json:"lead"`
	Worker   string `json:"worker"`
	Reviewer string `json:"reviewer"`
}

// Config is the persisted fleet configuration.
type Config struct {
	Version int     `json:"version"`
	Spaces  []Space `json:"spaces"`
	Budget  Budget  `json:"budget"`
	Models  Models  `json:"models"`
}

// ErrBudgetExceeded is returned when a spend would exceed the budget.
var ErrBudgetExceeded = errors.New("budget exceeded")

// Default returns a config populated with default values.
func Default() Config {
	return Config{
		Version: CurrentVersion,
		Spaces:  []Space{},
		Models: Models{
			Lead:     DefaultLeadModel,
			Worker:   DefaultWorkerModel,
			Reviewer: DefaultReviewerModel,
		},
	}
}

// Validate checks structural invariants.
func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d (want %d)", c.Version, CurrentVersion)
	}
	if c.Models.Lead == "" || c.Models.Worker == "" || c.Models.Reviewer == "" {
		return errors.New("models: lead, worker and reviewer must be set")
	}
	if bad(c.Budget.MaxBudgetUSD) || bad(c.Budget.CurrentSpendUSD) {
		return errors.New("budget: values must be finite and non-negative")
	}
	seen := map[string]bool{}
	for _, s := range c.Spaces {
		if err := ValidateID(s.ID); err != nil {
			return fmt.Errorf("space: %w", err)
		}
		if seen[s.ID] {
			return fmt.Errorf("duplicate space id %q", s.ID)
		}
		seen[s.ID] = true
		if bad(s.MaxBudgetUSD) || bad(s.TaskBudgetUSD) {
			return fmt.Errorf("space %q: budgets must be finite and non-negative", s.ID)
		}
		pseen := map[string]bool{}
		for _, p := range s.Projects {
			if err := ValidateID(p.ID); err != nil {
				return fmt.Errorf("space %q project: %w", s.ID, err)
			}
			if pseen[p.ID] {
				return fmt.Errorf("space %q: duplicate project id %q", s.ID, p.ID)
			}
			pseen[p.ID] = true
			if !filepath.IsAbs(p.Path) {
				return fmt.Errorf("space %q project %q: path must be absolute", s.ID, p.ID)
			}
		}
	}
	return nil
}

func bad(v float64) bool { return v < 0 || math.IsNaN(v) || math.IsInf(v, 0) }

// Space returns the space with the given ID.
func (c *Config) Space(id string) (*Space, bool) {
	for i := range c.Spaces {
		if c.Spaces[i].ID == id {
			return &c.Spaces[i], true
		}
	}
	return nil, false
}

// Project returns the project with the given ID within the space.
func (s *Space) Project(id string) (*Project, bool) {
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i], true
		}
	}
	return nil, false
}

// Remaining returns the remaining budget; ok is false when unlimited.
func (b Budget) Remaining() (remaining float64, ok bool) {
	if b.MaxBudgetUSD == 0 {
		return 0, false
	}
	return math.Max(0, b.MaxBudgetUSD-b.CurrentSpendUSD), true
}

// Exceeded reports whether spend has reached the limit.
func (b Budget) Exceeded() bool {
	return b.MaxBudgetUSD > 0 && b.CurrentSpendUSD >= b.MaxBudgetUSD
}

// AddSpend records spend. It refuses negative amounts and amounts that would
// exceed the budget (leaving the budget unchanged).
func (b *Budget) AddSpend(usd float64) error {
	if bad(usd) {
		return fmt.Errorf("invalid spend %v", usd)
	}
	if b.MaxBudgetUSD > 0 && b.CurrentSpendUSD+usd > b.MaxBudgetUSD {
		return fmt.Errorf("%w: %.4f + %.4f > %.4f", ErrBudgetExceeded, b.CurrentSpendUSD, usd, b.MaxBudgetUSD)
	}
	b.CurrentSpendUSD += usd
	return nil
}

// Load reads and validates a config file. A missing file yields Default().
// Unknown fields are rejected.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c := Default()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config %s: trailing data", path)
	}
	if c.Spaces == nil {
		c.Spaces = []Space{}
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// Save validates and atomically writes the config as indented JSON.
func Save(path string, c Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// WriteFileAtomic writes data to path via a temp file in the same directory,
// fsyncs it, renames over the target, then fsyncs the directory.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
