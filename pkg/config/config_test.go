package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() Config {
	c := Default()
	c.Budget = Budget{MaxBudgetUSD: 10, CurrentSpendUSD: 1.5}
	c.Spaces = []Space{{ID: "s1", Name: "Space", Projects: []Project{{ID: "p1", Name: "P", Path: "/tmp/p1", BaseBranch: "main"}}}}
	return c
}

func TestDefault(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Models.Lead == "" || c.Models.Worker == "" || c.Models.Reviewer == "" {
		t.Fatalf("%+v", c.Models)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	want := sample()
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("mismatch\n%s\n%s", a, b)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm %v", st.Mode().Perm())
	}
}

func TestLoadMissingReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || c.Version != CurrentVersion || c.Spaces == nil {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"unknown field": `{"version":1,"bogus":1}`,
		"trailing":      `{"version":1} {}`,
		"bad version":   `{"version":99}`,
		"malformed":     `{`,
		"empty model":   `{"version":1,"models":{"lead":"","worker":"w","reviewer":"r"}}`,
		"neg budget":    `{"version":1,"budget":{"max_budget_usd":-1}}`,
		"rel path":      `{"version":1,"spaces":[{"id":"s","name":"s","projects":[{"id":"p","name":"p","path":"rel"}]}]}`,
		"dup space":     `{"version":1,"spaces":[{"id":"s","name":"a","projects":[]},{"id":"s","name":"b","projects":[]}]}`,
		"bad space id":  `{"version":1,"spaces":[{"id":"../x","name":"a","projects":[]}]}`,
	}
	for name, body := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadPartialUsesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"version":1,"budget":{"max_budget_usd":5}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Models.Worker != DefaultWorkerModel || c.Budget.MaxBudgetUSD != 5 {
		t.Fatalf("%+v", c)
	}
}

func TestSaveRejectsInvalidAndKeepsOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	bad := sample()
	bad.Budget.MaxBudgetUSD = -1
	if err := Save(path, bad); err == nil {
		t.Fatal("expected error")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("file modified by failed save")
	}
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	for i := 0; i < 3; i++ {
		if err := WriteFileAtomic(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func TestWriteFileAtomicFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	// Target is a non-empty directory: rename must fail.
	target := filepath.Join(dir, "t")
	os.MkdirAll(filepath.Join(target, "child"), 0o755)
	if err := WriteFileAtomic(target, []byte("x"), 0o600); err == nil {
		t.Fatal("expected error")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp file leaked: %v", ents)
	}
}

func TestBudget(t *testing.T) {
	b := Budget{MaxBudgetUSD: 10, CurrentSpendUSD: 4}
	if r, ok := b.Remaining(); !ok || r != 6 {
		t.Fatalf("%v %v", r, ok)
	}
	if err := b.AddSpend(5); err != nil || b.CurrentSpendUSD != 9 {
		t.Fatalf("%v %+v", err, b)
	}
	if err := b.AddSpend(2); !errors.Is(err, ErrBudgetExceeded) || b.CurrentSpendUSD != 9 {
		t.Fatalf("%v %+v", err, b)
	}
	if err := b.AddSpend(-1); err == nil {
		t.Fatal("negative spend accepted")
	}
	if err := b.AddSpend(1); err != nil || !b.Exceeded() {
		t.Fatalf("%v %+v", err, b)
	}
	if r, _ := b.Remaining(); r != 0 {
		t.Fatal(r)
	}
}

func TestBudgetUnlimited(t *testing.T) {
	b := Budget{}
	if _, ok := b.Remaining(); ok {
		t.Fatal("expected unlimited")
	}
	if err := b.AddSpend(1e6); err != nil || b.Exceeded() {
		t.Fatalf("%v", err)
	}
}

func TestLookup(t *testing.T) {
	c := sample()
	s, ok := c.Space("s1")
	if !ok {
		t.Fatal("space missing")
	}
	if p, ok := s.Project("p1"); !ok || p.Path != "/tmp/p1" {
		t.Fatal("project missing")
	}
	if _, ok := c.Space("zz"); ok {
		t.Fatal("unexpected space")
	}
	if _, ok := s.Project("zz"); ok {
		t.Fatal("unexpected project")
	}
}

func TestSpaceBudgetsValidateAndRoundTrip(t *testing.T) {
	c := sample()
	c.Spaces[0].MaxBudgetUSD, c.Spaces[0].TaskBudgetUSD = 5, 1.25
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Spaces[0].MaxBudgetUSD != 5 || got.Spaces[0].TaskBudgetUSD != 1.25 {
		t.Fatalf("round trip: %+v %v", got.Spaces, err)
	}
	for _, mod := range []func(*Space){
		func(s *Space) { s.MaxBudgetUSD = -1 },
		func(s *Space) { s.TaskBudgetUSD = -0.5 },
	} {
		bad := sample()
		mod(&bad.Spaces[0])
		if err := bad.Validate(); err == nil {
			t.Errorf("negative budget accepted: %+v", bad.Spaces[0])
		}
	}
}
