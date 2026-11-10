package budget

import (
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fleet/pkg/agent"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

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
		t.Errorf("web/t1 cost = %v, want 1.5", got)
	}
	if got := tr.SpaceUsage("web").Cost.Total; !approx(got, 3.75) {
		t.Errorf("web total = %v, want 3.75", got)
	}
	if got := tr.SpaceUsage("api").Cost.Total; !approx(got, 9.00) {
		t.Errorf("api total = %v, want 9.0", got)
	}
	if got := tr.SpaceUsage("ghost").Cost.Total; got != 0 {
		t.Errorf("ghost total = %v, want 0", got)
	}

	mu := tr.ModelUsage("web", "t1")
	if len(mu) != 2 || mu[0].Model != "sonnet" || !approx(mu[0].Cost.Total, 1.0) {
		t.Errorf("web/t1 models = %+v", mu)
	}
	mu = tr.ModelUsage("web", "")
	if len(mu) != 3 || mu[0].Model != "sonnet" || !approx(mu[0].Cost.Total, 3.0) {
		t.Errorf("web models = %+v", mu)
	}

	ents := tr.Entries()
	if len(ents) != 5 {
		t.Fatalf("entries = %d, want 5", len(ents))
	}
	for i := 1; i < len(ents); i++ {
		prev := ents[i-1].Space + "/" + ents[i-1].Task + "/" + ents[i-1].Session
		cur := ents[i].Space + "/" + ents[i].Task + "/" + ents[i].Session
		if prev >= cur {
			t.Errorf("entries not sorted: %s >= %s", prev, cur)
		}
	}
}

func TestTrackerRecordIdempotentAndCopies(t *testing.T) {
	tr := NewTracker(Budgets{})
	st := stats("/s/a", 1.0, "m")
	if err := tr.Record("web", "t1", agent.RoleWorker, st); err != nil {
		t.Fatal(err)
	}
	st.Total.Cost.Total = 99
	st.ByModel["m"].Cost.Total = 99
	if got := tr.TaskUsage("web", "t1").Cost.Total; !approx(got, 1.0) {
		t.Errorf("Record did not copy usage: %v", got)
	}
	if err := tr.Record("web", "t1", agent.RoleWorker, stats("/s/a", 2.5, "m")); err != nil {
		t.Fatal(err)
	}
	if got := tr.TaskUsage("web", "t1").Cost.Total; !approx(got, 2.5) {
		t.Errorf("Record did not replace by session: %v", got)
	}
	if len(tr.Entries()) != 1 {
		t.Errorf("expected 1 entry, got %d", len(tr.Entries()))
	}
}

func TestBudgetStates(t *testing.T) {
	b := Budgets{Space: 10, Task: 2, WarnRatio: 0.5}
	tr := NewTracker(b)
	must := func(s, tk string, c float64) {
		if err := tr.Record(s, tk, agent.RoleWorker, stats("/s/"+tk, c, "m")); err != nil {
			t.Fatal(err)
		}
	}
	must("sp", "t1", 0.5)
	must("sp", "t2", 1.2)
	must("sp", "t3", 2.2)

	st := tr.Check("sp", "t1")
	if len(st) != 2 || st[0].State != BudgetOK || !approx(st[0].Remaining(), 1.5) {
		t.Errorf("t1 status = %+v", st)
	}
	st = tr.Check("sp", "t2")
	if len(st) != 2 || st[0].State != BudgetWarning {
		t.Errorf("t2 status = %+v", st)
	}
	st = tr.Check("sp", "t3")
	if len(st) != 2 || st[0].State != BudgetExceeded {
		t.Errorf("t3 status = %+v", st)
	}

	if err := tr.Enforce("sp", "t1"); err != nil {
		t.Errorf("t1 should pass: %v", err)
	}
	if err := tr.Enforce("sp", "t3"); !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("t3 error = %v, want ErrBudgetExceeded", err)
	}
}

func TestTrackerLedgerRoundTrip(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "state", "ledger.json")

	tr := NewTracker(Budgets{Task: 1})
	if err := tr.Record("sp", "t", agent.RoleWorker, stats("/s/1", 0.4, "m")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded := NewTracker(Budgets{})
	if err := loaded.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := loaded.TaskUsage("sp", "t").Cost.Total; !approx(got, 0.4) {
		t.Errorf("loaded cost = %v", got)
	}
	if len(loaded.Entries()) != 1 || loaded.Entries()[0].Session != "/s/1" {
		t.Errorf("loaded entries = %+v", loaded.Entries())
	}
}

func TestTrackerConcurrent(t *testing.T) {
	tr := NewTracker(Budgets{Space: 100})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = tr.Record("sp", "t", agent.RoleWorker, stats(filepath.Join("s", string(rune('a'+n))), 0.01, "m"))
				_ = tr.TaskUsage("sp", "t")
				_ = tr.Check("sp", "t")
			}
		}(i)
	}
	wg.Wait()
}
