package pi

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseSession(t *testing.T) {
	st, err := ParseSession(strings.NewReader(sampleSession()))
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != "sess-1" || st.CWD != "/work/repo" || st.Name != "my task" {
		t.Errorf("header: id=%q cwd=%q name=%q", st.ID, st.CWD, st.Name)
	}
	if st.Malformed != 1 {
		t.Errorf("malformed = %d, want 1 (torn last line)", st.Malformed)
	}
	if st.Messages != 6 { // u1 a1 a2 a3 a4 u2; the duplicate a2 is ignored
		t.Errorf("messages = %d, want 6", st.Messages)
	}

	wantCost := 0.10 + 0.50 + 0.015 + 0.01 + 0.02
	if !approx(st.Total.Cost.Total, wantCost) {
		t.Errorf("total cost = %v, want %v", st.Total.Cost.Total, wantCost)
	}
	// tokens: a1 1160, a2 300, usage 5000, compaction 15, a4 20
	if st.Total.TotalTokens != 1160+300+5000+15+20 {
		t.Errorf("total tokens = %d", st.Total.TotalTokens)
	}
	if st.Total.Input != 100+200+0+10+10 || st.Total.Output != 50+100+0+5+10 || st.Total.CacheRead != 1000+5000 || st.Total.CacheWrite != 10 {
		t.Errorf("token classes wrong: %+v", st.Total)
	}

	sonnet := st.ByModel["anthropic/claude-sonnet-4-5"]
	opus := st.ByModel["anthropic/claude-opus-5-5"]
	if sonnet == nil || opus == nil || len(st.ByModel) != 2 {
		t.Fatalf("models = %v", st.ByModel)
	}
	if !approx(sonnet.Cost.Total, 0.10+0.015+0.01+0.02) || sonnet.Requests != 4 {
		t.Errorf("sonnet = %+v", sonnet)
	}
	if !approx(opus.Cost.Total, 0.50) || opus.Requests != 1 {
		t.Errorf("opus = %+v (duplicate or zero-usage entry counted?)", opus)
	}
	if st.PrimaryModel() != "anthropic/claude-opus-5-5" {
		// opus 0.50 vs sonnet 0.145
		t.Errorf("primary = %q", st.PrimaryModel())
	}
	models := st.Models()
	if len(models) != 2 || models[0].Key() != "anthropic/claude-opus-5-5" {
		t.Errorf("Models() order = %+v", models)
	}

	start := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !st.Start.Equal(start) || !st.End.Equal(start.Add(4*time.Hour+6*time.Minute)) {
		t.Errorf("start=%v end=%v", st.Start, st.End)
	}
	if st.Duration != 4*time.Hour+6*time.Minute {
		t.Errorf("duration = %v", st.Duration)
	}
	// Active: 10:00:00 -> 10:05:00 (5m) + 14:05 -> 14:06 (1m); 4h gap excluded.
	if st.ActiveDuration != 6*time.Minute {
		t.Errorf("active = %v, want 6m", st.ActiveDuration)
	}
}

func TestParseSessionTotalsFallbacks(t *testing.T) {
	// No totalTokens / cost.total fields: derived from components.
	line := `{"type":"message","id":"a","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":10,"output":5,"cacheRead":1,"cacheWrite":2,"cost":{"input":0.1,"output":0.2,"cacheRead":0.01,"cacheWrite":0.02}}}}`
	st, err := ParseSession(strings.NewReader(line)) // no trailing newline
	if err != nil {
		t.Fatal(err)
	}
	if st.Total.TotalTokens != 18 || !approx(st.Total.Cost.Total, 0.33) {
		t.Errorf("fallbacks: %+v", st.Total)
	}
}

func TestParseSessionModelFallbackAndUnknown(t *testing.T) {
	// Assistant message without model uses the latest model_change; with no
	// model_change at all it is attributed to "unknown".
	noModel := `{"type":"message","id":"x","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","usage":` + usageJSON(1, 1, 0, 0, 0.1) + `}}`
	st, _ := ParseSession(strings.NewReader(noModel))
	if st.ByModel["unknown"] == nil {
		t.Errorf("want unknown, got %v", st.ByModel)
	}
	withChange := `{"type":"model_change","id":"m","timestamp":"2026-01-01T00:00:00Z","provider":"prov","modelId":"mod"}` + "\n" + noModel
	st, _ = ParseSession(strings.NewReader(withChange))
	if st.ByModel["prov/mod"] == nil {
		t.Errorf("want prov/mod, got %v", st.ByModel)
	}
	// Model without provider.
	if got := (ModelUsage{Model: "m"}).Key(); got != "m" {
		t.Errorf("key = %q", got)
	}
}

func TestParseSessionToolResultUsage(t *testing.T) {
	l := `{"type":"message","id":"t","timestamp":"2026-01-01T00:00:00Z","message":{"role":"toolResult","usage":` + usageJSON(1, 1, 0, 0, 0.25) + `}}`
	st, _ := ParseSession(strings.NewReader(l))
	if !approx(st.Total.Cost.Total, 0.25) {
		t.Errorf("nested tool usage dropped: %+v", st.Total)
	}
}

func TestParseSessionEmptyAndGarbage(t *testing.T) {
	st, err := ParseSession(strings.NewReader(""))
	if err != nil || !st.Total.IsZero() || st.Duration != 0 {
		t.Errorf("empty: %+v %v", st, err)
	}
	st, err = ParseSession(strings.NewReader("not json\n\n  \n{}\n[1,2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Malformed != 2 { // "not json" and "[1,2]" (array into struct)
		t.Errorf("malformed = %d", st.Malformed)
	}
}

func TestParseSessionHugeLine(t *testing.T) {
	// System-prompt entries can be far larger than bufio.Scanner's default.
	huge := `{"type":"message","id":"s","timestamp":"2026-01-01T00:00:00Z","message":{"role":"system","content":"` + strings.Repeat("x", 3<<20) + `"}}`
	st, err := ParseSession(strings.NewReader(huge + "\n" + assistantLine("a", "2026-01-01T00:00:01Z", "p", "m", usageJSON(1, 1, 0, 0, 0.5)) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !approx(st.Total.Cost.Total, 0.5) || st.Malformed != 0 {
		t.Errorf("huge line broke parsing: %+v", st)
	}
}

func TestParseSessionFile(t *testing.T) {
	p := writeFile(t, filepath.Join(t.TempDir(), "s.jsonl"), sampleSession())
	st, err := ParseSessionFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Path != p {
		t.Errorf("path = %q", st.Path)
	}
	if _, err := ParseSessionFile(p + ".missing"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseSessionDirAndSum(t *testing.T) {
	d := t.TempDir()
	s1 := `{"type":"session","id":"s1","timestamp":"2026-01-02T00:00:00Z"}` + "\n" + assistantLine("a", "2026-01-02T00:01:00Z", "p", "m1", usageJSON(1, 1, 0, 0, 0.1))
	s2 := `{"type":"session","id":"s2","timestamp":"2026-01-01T00:00:00Z"}` + "\n" + assistantLine("a", "2026-01-01T00:02:00Z", "p", "m1", usageJSON(2, 2, 0, 0, 0.2))
	writeFile(t, filepath.Join(d, "a", "2026-01-02_s1.jsonl"), s1)
	writeFile(t, filepath.Join(d, "archive", "2026-01-01_s2.jsonl"), s2)
	writeFile(t, filepath.Join(d, "notes.txt"), "ignored")

	got, err := ParseSessionDir(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "s2" || got[1].ID != "s1" {
		t.Fatalf("sessions not sorted by start: %+v", got)
	}
	sum := SumSessions(got)
	if !approx(sum.Total.Cost.Total, 0.3) || sum.ByModel["p/m1"].Requests != 2 {
		t.Errorf("sum = %+v", sum.Total)
	}
	if !sum.Start.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) || sum.Duration != 3*time.Minute {
		t.Errorf("sum start=%v duration=%v", sum.Start, sum.Duration)
	}

	none, err := ParseSessionDir(filepath.Join(d, "missing"))
	if err != nil || len(none) != 0 {
		t.Errorf("missing dir: %v %v", none, err)
	}
}

func TestDefaultSessionsDir(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("HOME", "/home/u")
	if got, _ := DefaultSessionsDir(); got != "/home/u/.pi/agent/sessions" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "/cfg")
	if got, _ := DefaultSessionsDir(); got != "/cfg/sessions" {
		t.Errorf("agent dir = %q", got)
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "/sess")
	if got, _ := DefaultSessionsDir(); got != "/sess" {
		t.Errorf("session dir = %q", got)
	}
}

func TestUsageAddAndZero(t *testing.T) {
	var u Usage
	if !u.IsZero() {
		t.Error("zero usage not zero")
	}
	u.Add(Usage{Input: 1, Output: 2, Reasoning: 1, Cost: Cost{Total: 0.5, Input: 0.2}})
	u.Add(Usage{Input: 1, CacheRead: 3, CacheWrite: 4, TotalTokens: 9, Cost: Cost{Total: 0.5, Output: 0.3, CacheRead: 0.1, CacheWrite: 0.2}})
	want := Usage{Input: 2, Output: 2, CacheRead: 3, CacheWrite: 4, Reasoning: 1, TotalTokens: 9,
		Cost: Cost{Input: 0.2, Output: 0.3, CacheRead: 0.1, CacheWrite: 0.2, Total: 1}}
	if u != want {
		t.Errorf("got %+v want %+v", u, want)
	}
}
