package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultTiers(t *testing.T) {
	cs := DefaultConfigs()
	if err := cs.Validate(); err != nil {
		t.Fatalf("default configs invalid: %v", err)
	}
	tests := []struct {
		role Role
		tier Tier
		sub  string
	}{
		{RoleLead, TierCheap, "haiku"},
		{RoleWorker, TierMedium, "sonnet"},
		{RoleReviewer, TierSmart, "opus"},
	}
	for _, tc := range tests {
		c, err := cs.For(tc.role)
		if err != nil {
			t.Fatal(err)
		}
		if c.Tier != tc.tier {
			t.Errorf("%s tier = %s, want %s", tc.role, c.Tier, tc.tier)
		}
		if !strings.Contains(c.ModelPattern(), tc.sub) {
			t.Errorf("%s model = %q, want it to contain %q", tc.role, c.ModelPattern(), tc.sub)
		}
		if c.InheritUserContext {
			t.Errorf("%s must not inherit user context by default", tc.role)
		}
	}
	if !cs.Lead.Persistent || cs.Worker.Persistent || cs.Reviewer.Persistent {
		t.Error("only the lead is persistent")
	}
	if got := cs.Reviewer.Tools; len(got) == 0 {
		t.Error("reviewer must have a read-only tool allowlist")
	}
	for _, tool := range cs.Reviewer.Tools {
		if tool == "bash" || tool == "edit" || tool == "write" {
			t.Errorf("reviewer has mutating tool %q", tool)
		}
	}
	if _, err := cs.For("nope"); err == nil {
		t.Error("For(unknown) should fail")
	}
}

func TestModelOverride(t *testing.T) {
	c := DefaultConfig(RoleWorker)
	c.Model = "provider/custom"
	if c.ModelPattern() != "provider/custom" {
		t.Errorf("override ignored: %q", c.ModelPattern())
	}
	c.Model = ""
	if c.ModelPattern() != DefaultModels[TierMedium] {
		t.Errorf("tier default not used: %q", c.ModelPattern())
	}
}

func TestConfigValidate(t *testing.T) {
	mut := func(role Role, f func(*Config)) Config {
		c := DefaultConfig(role)
		f(&c)
		return c
	}
	tests := []struct {
		name string
		c    Config
		want string // substring of error, "" for ok
	}{
		{"ok", DefaultConfig(RoleWorker), ""},
		{"bad role", Config{Role: "x", Tier: TierCheap}, "unknown role"},
		{"bad tier", mut(RoleWorker, func(c *Config) { c.Tier = "x"; c.Model = "m" }), "unknown tier"},
		{"negative timeout", mut(RoleWorker, func(c *Config) { c.Timeout = -time.Second }), "negative"},
		{"worker inherits", mut(RoleWorker, func(c *Config) { c.InheritUserContext = true }), "must not inherit"},
		{"reviewer inherits", mut(RoleReviewer, func(c *Config) { c.InheritUserContext = true }), "must not inherit"},
		{"worker persistent", mut(RoleWorker, func(c *Config) { c.Persistent = true; c.Cleanup = CleanupPolicy{} }), "ephemeral"},
		{"lead not persistent", mut(RoleLead, func(c *Config) { c.Persistent = false }), "persistent"},
		{"lead with cleanup", mut(RoleLead, func(c *Config) { c.Cleanup.RemoveSession = []Outcome{OutcomeSucceeded} }), "cleanup"},
		{"lead may inherit", mut(RoleLead, func(c *Config) { c.InheritUserContext = true }), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestConfigsValidateRoleMismatch(t *testing.T) {
	cs := DefaultConfigs()
	cs.Worker = DefaultConfig(RoleReviewer)
	if err := cs.Validate(); err == nil {
		t.Fatal("expected role mismatch error")
	}
}

func TestDefaultConfigPanicsOnUnknownRole(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	DefaultConfig("nope")
}

func TestEnsureLeadSkill(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := EnsureLeadSkill()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != LeadSkillContent() || !strings.HasPrefix(string(got), "---\nname: fleet-lead\n") {
		t.Fatalf("unexpected skill content:\n%s", got)
	}

	// Idempotent: unchanged content is not rewritten.
	before, _ := os.Stat(file)
	if _, err := EnsureLeadSkill(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(file)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("skill rewritten although unchanged")
	}

	// Stale content is repaired.
	if err := os.WriteFile(file, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureLeadSkill(); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(file)
	if string(got) != LeadSkillContent() {
		t.Error("stale skill not repaired")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// The embedded lead skill is a copy of the repository's canonical skill file.
func TestEmbeddedLeadSkillMatchesRepoSkill(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "skills", "fleet-lead", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if LeadSkillContent() != string(want) {
		t.Error("pkg/agent/skills/fleet-lead/SKILL.md differs from skills/fleet-lead/SKILL.md; copy the latter over the former")
	}
}

func TestEnsureReviewerSkill(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := EnsureReviewerSkill()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "SKILL.md")
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ReviewerSkillContent() || !strings.HasPrefix(string(got), "---\nname: fleet-reviewer\n") {
		t.Fatalf("unexpected skill content:\n%s", got)
	}
}

// The embedded reviewer skill is a copy of the repository's canonical skill file.
func TestEmbeddedReviewerSkillMatchesRepoSkill(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("..", "..", "skills", "fleet-reviewer", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if ReviewerSkillContent() != string(want) {
		t.Error("pkg/agent/skills/fleet-reviewer/SKILL.md differs from skills/fleet-reviewer/SKILL.md; copy the latter over the former")
	}
}
