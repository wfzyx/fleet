// Package agent defines the roles that make up a fleet (Lead, Worker,
// Reviewer): their model tiers, tool and isolation settings, prompt templates,
// the structured review verdict, and ephemeral cleanup rules.
//
// The package is pure configuration and rendering; launching Pi processes is
// done by package pi, which imports this one.
package agent

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Role identifies an agent role within a space.
type Role string

const (
	RoleLead     Role = "lead"
	RoleWorker   Role = "worker"
	RoleReviewer Role = "reviewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleLead, RoleWorker, RoleReviewer:
		return true
	}
	return false
}

// Tier is a model capability/cost class.
type Tier string

const (
	// TierCheap is for fast, inexpensive routing models (haiku / flash).
	TierCheap Tier = "cheap"
	// TierMedium is for day-to-day implementation models (sonnet).
	TierMedium Tier = "medium"
	// TierSmart is for the most capable models (opus).
	TierSmart Tier = "smart"
)

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool {
	switch t {
	case TierCheap, TierMedium, TierSmart:
		return true
	}
	return false
}

// DefaultModels maps each tier to the Pi model pattern used by default.
// Patterns are resolved by Pi (`--model`), so "haiku" or "provider/id" both work.
var DefaultModels = map[Tier]string{
	TierCheap:  "haiku",
	TierMedium: "sonnet",
	TierSmart:  "claude-opus-5-5",
}

// DefaultTier is the tier each role runs on unless configured otherwise.
var DefaultTier = map[Role]Tier{
	RoleLead:     TierCheap,
	RoleWorker:   TierMedium,
	RoleReviewer: TierSmart,
}

// Config describes how one role runs.
type Config struct {
	Role Role `json:"role"`
	Tier Tier `json:"tier"`
	// Model overrides the tier's default model pattern when non-empty.
	Model string `json:"model,omitempty"`
	// Thinking is the Pi thinking level (off, minimal, low, medium, high, xhigh, max).
	Thinking string `json:"thinking,omitempty"`
	// Tools is an allowlist passed to `pi --tools`. Empty means Pi's defaults.
	Tools []string `json:"tools,omitempty"`
	// Persistent roles keep a resumable session; others are one-shot.
	Persistent bool `json:"persistent"`
	// InheritUserContext lets Pi load the user's extensions, skills, prompt
	// templates and global context files (including episodic memory and dream
	// files). It must stay false for Worker and Reviewer.
	InheritUserContext bool `json:"inherit_user_context"`
	// Timeout bounds a one-shot run. Zero means no limit.
	Timeout time.Duration `json:"timeout,omitempty"`
	// MaxDiffBytes caps the diff embedded in a reviewer prompt (reviewer only).
	MaxDiffBytes int `json:"max_diff_bytes,omitempty"`
	// Cleanup says what to remove once the role finishes.
	Cleanup CleanupPolicy `json:"cleanup"`
}

// DefaultConfig returns the built-in configuration for a role. It panics on
// an unknown role, which is a programming error.
func DefaultConfig(role Role) Config {
	tier, ok := DefaultTier[role]
	if !ok {
		panic(fmt.Sprintf("agent: unknown role %q", role))
	}
	c := Config{Role: role, Tier: tier, Model: DefaultModels[tier]}
	switch role {
	case RoleLead:
		c.Thinking = "low"
		c.Persistent = true
		c.Cleanup = CleanupPolicy{} // the lead is never cleaned up
	case RoleWorker:
		c.Thinking = "medium"
		c.Timeout = 2 * time.Hour
		c.Cleanup = CleanupPolicy{
			RemoveWorktree: []Outcome{OutcomeSucceeded, OutcomeCancelled},
			RemoveSession:  []Outcome{OutcomeSucceeded},
		}
	case RoleReviewer:
		c.Thinking = "high"
		c.Tools = []string{"read", "grep", "find", "ls"} // read-only
		c.Timeout = 30 * time.Minute
		c.MaxDiffBytes = 400 * 1024
		c.Cleanup = CleanupPolicy{
			RemoveSession: []Outcome{OutcomeSucceeded, OutcomeCancelled},
		}
	}
	return c
}

// Validate checks that the configuration is coherent and that isolation
// invariants hold.
func (c Config) Validate() error {
	if !c.Role.Valid() {
		return fmt.Errorf("agent: unknown role %q", c.Role)
	}
	if !c.Tier.Valid() {
		return fmt.Errorf("agent: role %s: unknown tier %q", c.Role, c.Tier)
	}
	if c.ModelPattern() == "" {
		return fmt.Errorf("agent: role %s: no model for tier %q", c.Role, c.Tier)
	}
	if c.Timeout < 0 {
		return fmt.Errorf("agent: role %s: negative timeout", c.Role)
	}
	if c.Role != RoleLead && c.InheritUserContext {
		return fmt.Errorf("agent: role %s must not inherit user context (memories, dreams)", c.Role)
	}
	if c.Role != RoleLead && c.Persistent {
		return fmt.Errorf("agent: role %s is ephemeral and cannot be persistent", c.Role)
	}
	if c.Role == RoleLead && !c.Persistent {
		return fmt.Errorf("agent: lead must be persistent")
	}
	if c.Persistent && !c.Cleanup.IsZero() {
		return fmt.Errorf("agent: role %s is persistent and must not have cleanup rules", c.Role)
	}
	return nil
}

// ModelPattern returns the Pi model pattern to use: the explicit override or
// the tier default.
func (c Config) ModelPattern() string {
	if c.Model != "" {
		return c.Model
	}
	return DefaultModels[c.Tier]
}

// Configs holds the configuration of every role.
type Configs struct {
	Lead     Config `json:"lead"`
	Worker   Config `json:"worker"`
	Reviewer Config `json:"reviewer"`
}

// DefaultConfigs returns the default configuration for all roles.
func DefaultConfigs() Configs {
	return Configs{
		Lead:     DefaultConfig(RoleLead),
		Worker:   DefaultConfig(RoleWorker),
		Reviewer: DefaultConfig(RoleReviewer),
	}
}

// For returns the configuration of the given role.
func (cs Configs) For(role Role) (Config, error) {
	switch role {
	case RoleLead:
		return cs.Lead, nil
	case RoleWorker:
		return cs.Worker, nil
	case RoleReviewer:
		return cs.Reviewer, nil
	}
	return Config{}, fmt.Errorf("agent: unknown role %q", role)
}

// Validate validates every role and that each config sits under its own role.
func (cs Configs) Validate() error {
	for _, p := range []struct {
		want Role
		c    Config
	}{{RoleLead, cs.Lead}, {RoleWorker, cs.Worker}, {RoleReviewer, cs.Reviewer}} {
		if p.c.Role != p.want {
			return fmt.Errorf("agent: config for %s has role %q", p.want, p.c.Role)
		}
		if err := p.c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

//go:embed skills/fleet-lead/SKILL.md
var skillFS embed.FS

// LeadSkillName is the name of the skill every lead loads.
const LeadSkillName = "fleet-lead"

// LeadSkillContent returns the embedded fleet-lead skill.
func LeadSkillContent() string {
	b, err := skillFS.ReadFile("skills/fleet-lead/SKILL.md")
	if err != nil { // embedded at build time; cannot happen
		panic(err)
	}
	return string(b)
}

// EnsureLeadSkill materialises the embedded fleet-lead skill under
// $XDG_DATA_HOME/fleet/skills/fleet-lead and returns that directory. The file
// is only rewritten when its content differs.
func EnsureLeadSkill() (string, error) {
	root, err := SkillsDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, LeadSkillName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agent: create skill dir: %w", err)
	}
	file := filepath.Join(dir, "SKILL.md")
	want := LeadSkillContent()
	if cur, err := os.ReadFile(file); err == nil && string(cur) == want {
		return dir, nil
	}
	tmp, err := os.CreateTemp(dir, ".SKILL.md.*")
	if err != nil {
		return "", fmt.Errorf("agent: write skill: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(want); err != nil {
		tmp.Close()
		return "", fmt.Errorf("agent: write skill: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agent: write skill: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return "", fmt.Errorf("agent: install skill: %w", err)
	}
	return dir, nil
}
