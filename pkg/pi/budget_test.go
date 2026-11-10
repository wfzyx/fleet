package pi

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fleet/pkg/agent"
)

func stats(path string, cost float64, model string) *SessionStats {
	u := Usage{Input: 10, Output: 10, TotalTokens: 20, Cost: Cost{Total: cost}}
	return &SessionStats{
		Path: path, Total: u, Duration: time.Minute, ActiveDuration: 30 * time.Second,
		ByModel: map[string]*ModelUsage{model: {Provider: "p", Model: model, Requests: 1, Usage: u}},
	}
}

func TestTrackerAccumulates(t *testing.T) {
	tr := NewTracker(Budgets{})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tr.Record("web", "t1", agent.RoleWorker, stats("/s/w1", 1.00, "sonnet")))
	must(tr.Record("web", "t1", agent.RoleReviewer, stats("/s/r1", 0.50, "opus")))
	must(tr.Record("web", "t2", agent.RoleWorker, stats("/s/w2", 2.00, "sonnet")))
	must(tr.Record("web", "", agent.RoleLead, stats("/s/l", 0.25, "haiku")))
	must(tr.Record("api", "t1", agent.RoleWorker, stats("/s/w3", 9.00, "sonnet")))

	if got := tr.TaskUsage("web", "t1").Cost.Total; !approx(got, 1.5) {
		t.Errorf("web/t1 = %v", got)
	}
	if got := tr.TaskUsage("api", "t1").Cost.Total; !approx(got, 9) {
		t.Errorf("api/t1 = %v (spaces must not mix)", got)
	}
	if got := tr.SpaceUsage("web"); !approx(got.Cost.Total, 3.75) || got.TotalTokens != 80 {
		t.Errorf("web space = %+v", got)
	}
	if got := tr.SpaceUsage("none"); !got.IsZero() {
		t.Errorf("unknown space = %+v", got)
	}

	mu := tr.ModelUsage("web", "")
	if len(mu) != 3 || mu[0].Model != "sonnet" || !approx(mu[0].Cost.Total, 3.0) {
		t.Errorf("space models = %+v", mu)
	}
	mu = tr.ModelUsage("web", "t1")
	if len(mu) != 2 || mu[0].Model != "sonnet" || mu[1].Model != "opus" {
		t.Errorf("task models = %+v", mu)
	}
	if len(tr.Entries()) != 5 {
		t.Errorf("entries = %d", len(tr.Entries()))
	}
}

func TestTrackerRecordIdempotentAndCopies(t *testing.T) {
	tr := NewTracker(Budgets{})
	st := stats("/s/w1", 1, "m")
	_ = tr.Record("sp", "t", agent.RoleWorker, st)
	// The session grew; re-recording replaces rather than adds.
	st2 := stats("/s/w1", 1.5, "m")
	_ = tr.Record("sp", "t", agent.RoleWorker, st2)
	if got := tr.TaskUsage("sp", "t").Cost.Total; !approx(got, 1.5) {
		t.Errorf("cost = %v, want 1.5", got)
	}
	// Mutating the source stats afterwards must not change the ledger.
	st2.ByModel["m"].Cost.Total = 100
	if got := tr.ModelUsage("sp", "t")[0].Cost.Total; !approx(got, 1.5) {
		t.Errorf("tracker aliases caller data: %v", got)
	}
}

func TestTrackerRecordValidation(t *testing.T) {
	tr := NewTracker(Budgets{})
	if err := tr.Record("../x", "t", agent.RoleWorker, stats("/p", 1, "m")); !errors.Is(err, agent.ErrInvalidName) {
		t.Errorf("bad space: %v", err)
	}
	if err := tr.Record("s", "a/b", agent.RoleWorker, stats("/p", 1, "m")); !errors.Is(err, agent.ErrInvalidName) {
		t.Errorf("bad task: %v", err)
	}
	if err := tr.Record("s", "t", agent.RoleWorker, nil); err == nil {
		t.Error("nil stats accepted")
	}
	if err := tr.Record("s", "t", agent.RoleWorker, &SessionStats{}); err == nil {
		t.Error("stats without path/id accepted")
	}
	if err := tr.Record("s", "t", agent.RoleWorker, &SessionStats{ID: "only-id"}); err != nil {
		t.Errorf("id-only stats rejected: %v", err)
	}
}

func TestBudgetStates(t *testing.T) {
	b := Budgets{Task: 1.00, Space: 10.00}
	tr := NewTracker(b)
	check := func(space, task string) (BudgetStatus, BudgetStatus) {
		t.Helper()
		got := tr.Check(space, task)
		if len(got) != 2 {
			t.Fatalf("Check len = %d", len(got))
		}
		return got[0], got[1]
	}

	task, space := check("sp", "t")
	if task.State != BudgetOK || space.State != BudgetOK || task.Scope != ScopeTask || space.Scope != ScopeSpace {
		t.Errorf("fresh: %+v %+v", task, space)
	}
	if err := tr.Enforce("sp", "t"); err != nil {
		t.Errorf("fresh enforce: %v", err)
	}

	_ = tr.Record("sp", "t", agent.RoleWorker, stats("/a", 0.80, "m"))
	task, _ = check("sp", "t")
	if task.State != BudgetWarning || !approx(task.Remaining(), 0.2) {
		t.Errorf("at 80%%: %+v", task)
	}
	if err := tr.Enforce("sp", "t"); err != nil {
		t.Errorf("warning must not block: %v", err)
	}

	_ = tr.Record("sp", "t", agent.RoleReviewer, stats("/b", 0.20, "m"))
	task, _ = check("sp", "t")
	if task.State != BudgetExceeded || task.Remaining() > 0 {
		t.Errorf("at limit: %+v", task)
	}
	err := tr.Enforce("sp", "t")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("enforce = %v", err)
	}
	// Another task in the same space is unaffected.
	if err := tr.Enforce("sp", "other"); err != nil {
		t.Errorf("other task blocked: %v", err)
	}

	// Space budget trips on the aggregate of many tasks.
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		_ = tr.Record("sp", n, agent.RoleWorker, stats(filepath.Join("/x", n), 0.9, "m"))
	}
	// 1.0 (t) + 9*0.9 = 9.1 -> warning for space (>= 8.0), not exceeded yet
	_, space = check("sp", "z")
	if space.State != BudgetWarning {
		t.Errorf("space = %+v", space)
	}
	_ = tr.Record("sp", "", agent.RoleLead, stats("/lead", 1.0, "m"))
	err = tr.Enforce("sp", "z")
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("space limit not enforced: %v", err)
	}
	if err := tr.Enforce("elsewhere", "z"); err != nil {
		t.Errorf("other space affected: %v", err)
	}
}

func TestBudgetOverridesAndUnlimited(t *testing.T) {
	tr := NewTracker(Budgets{
		Task: 1, Space: 0, // space unlimited
		SpaceOverrides: map[string]float64{"big": 100},
		TaskOverrides:  map[string]float64{"sp/free": -1, "sp/pricey": 5},
		WarnRatio:      0.5,
	})
	_ = tr.Record("sp", "free", agent.RoleWorker, stats("/1", 50, "m"))
	_ = tr.Record("sp", "pricey", agent.RoleWorker, stats("/2", 3, "m"))
	_ = tr.Record("sp", "normal", agent.RoleWorker, stats("/3", 0.6, "m"))

	free := tr.Check("sp", "free")
	if free[0].State != BudgetUnlimited || free[0].Limit != 0 || free[0].Remaining() != 0 {
		t.Errorf("free = %+v", free[0])
	}
	if free[1].State != BudgetUnlimited {
		t.Errorf("space should be unlimited: %+v", free[1])
	}
	if got := tr.Check("sp", "pricey")[0]; got.State != BudgetWarning || got.Limit != 5 { // 3 >= 5*0.5
		t.Errorf("pricey = %+v", got)
	}
	if got := tr.Check("sp", "normal")[0]; got.State != BudgetWarning { // 0.6 >= 0.5
		t.Errorf("normal = %+v", got)
	}
	if got := tr.Check("big", "")[0]; got.Scope != ScopeSpace || got.Limit != 100 || len(tr.Check("big", "")) != 1 {
		t.Errorf("space-only check = %+v", got)
	}
	if err := tr.Enforce("sp", "free"); err != nil {
		t.Errorf("unlimited blocked: %v", err)
	}

	tr.SetBudgets(Budgets{Task: 0.1})
	if err := tr.Enforce("sp", "normal"); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("SetBudgets ignored: %v", err)
	}
	if (Budgets{WarnRatio: 7}).warnRatio() != DefaultWarnRatio {
		t.Error("out-of-range warn ratio accepted")
	}
}

func TestTrackerIngestDirAndLedgerRoundTrip(t *testing.T) {
	d := t.TempDir()
	writeFile(t, filepath.Join(d, "sessions", "a.jsonl"),
		`{"type":"session","id":"a","timestamp":"2026-01-01T00:00:00Z"}`+"\n"+assistantLine("m1", "2026-01-01T00:01:00Z", "p", "mod", usageJSON(5, 5, 0, 0, 0.4)))
	tr := NewTracker(Budgets{Task: 1})
	agg, err := tr.IngestDir("sp", "t", agent.RoleWorker, filepath.Join(d, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if !approx(agg.Total.Cost.Total, 0.4) {
		t.Errorf("agg = %+v", agg.Total)
	}
	// Ingesting twice must not double count.
	if _, err := tr.IngestDir("sp", "t", agent.RoleWorker, filepath.Join(d, "sessions")); err != nil {
		t.Fatal(err)
	}
	if got := tr.TaskUsage("sp", "t").Cost.Total; !approx(got, 0.4) {
		t.Errorf("double counted: %v", got)
	}
	// Missing dir is a no-op.
	if _, err := tr.IngestDir("sp", "t2", agent.RoleWorker, filepath.Join(d, "nope")); err != nil {
		t.Errorf("missing dir: %v", err)
	}

	ledger := filepath.Join(d, "state", "ledger.json")
	if err := tr.Save(ledger); err != nil {
		t.Fatal(err)
	}
	tr2 := NewTracker(Budgets{})
	if err := tr2.Load(ledger); err != nil {
		t.Fatal(err)
	}
	if got := tr2.TaskUsage("sp", "t"); !approx(got.Cost.Total, 0.4) || got.TotalTokens != 10 {
		t.Errorf("loaded = %+v", got)
	}
	if mu := tr2.ModelUsage("sp", "t"); len(mu) != 1 || mu[0].Key() != "p/mod" {
		t.Errorf("loaded models = %+v", mu)
	}
	// Reloaded ledger + re-ingest remains idempotent.
	if _, err := tr2.IngestDir("sp", "t", agent.RoleWorker, filepath.Join(d, "sessions")); err != nil {
		t.Fatal(err)
	}
	if got := tr2.TaskUsage("sp", "t").Cost.Total; !approx(got, 0.4) {
		t.Errorf("double counted after reload: %v", got)
	}
	// No temp files left next to the ledger.
	if m, _ := filepath.Glob(filepath.Join(d, "state", ".ledger.*")); len(m) != 0 {
		t.Errorf("temp files: %v", m)
	}
}

func TestLedgerLoadErrors(t *testing.T) {
	d := t.TempDir()
	tr := NewTracker(Budgets{})
	if err := tr.Load(filepath.Join(d, "missing.json")); err != nil {
		t.Errorf("missing ledger should be fine: %v", err)
	}
	if err := tr.Load(writeFile(t, filepath.Join(d, "bad.json"), "{nope")); err == nil {
		t.Error("corrupt ledger accepted")
	}
	if err := tr.Load(writeFile(t, filepath.Join(d, "v9.json"), `{"version":9,"entries":[]}`)); err == nil {
		t.Error("future ledger version accepted")
	}
}

func TestLedgerPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	if got, _ := LedgerPath(); got != "/data/fleet/ledger.json" {
		t.Errorf("got %q", got)
	}
}

func TestTrackerConcurrent(t *testing.T) {
	tr := NewTracker(Budgets{Task: 1000})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = tr.Record("sp", "t", agent.RoleWorker, stats(filepath.Join("/s", string(rune('a'+i%26)), string(rune('a'+i/26))), 1, "m"))
			_ = tr.Check("sp", "t")
			_ = tr.Entries()
		}(i)
	}
	wg.Wait()
	if got := tr.TaskUsage("sp", "t").Cost.Total; !approx(got, 50) {
		t.Errorf("total = %v", got)
	}
}
