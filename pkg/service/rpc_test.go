package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/pkg/config"
	"fleet/pkg/integrations"
	"fleet/pkg/budget"
	"fleet/pkg/driver"
	"fleet/pkg/schema"
)

func startServer(t *testing.T, e *env) (*Server, *Client) {
	t.Helper()
	srv := NewServer(e.svc, SocketPath(e.paths))
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("server close: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	c, err := Dial(bg, srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return srv, c
}

func TestSocketPathIsUnderXDGRuntimeDir(t *testing.T) {
	e := newEnv(t)
	want := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "fleet", "fleet.sock")
	if got := SocketPath(e.paths); got != want {
		t.Errorf("socket path = %s, want %s", got, want)
	}
}

func TestServerPermissionsAndPing(t *testing.T) {
	e := newEnv(t)
	srv, c := startServer(t, e)
	fi, err := os.Stat(srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v", fi.Mode())
	}
	di, _ := os.Stat(filepath.Dir(srv.Path()))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", di.Mode().Perm())
	}
	if err := c.Ping(bg); err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := c.Call(bg, "status", nil, &st); err != nil || st.Spaces != 1 || st.PID != os.Getpid() {
		t.Errorf("status = %+v %v", st, err)
	}
}

func TestServerRemovesSocketOnClose(t *testing.T) {
	e := newEnv(t)
	srv := NewServer(e.svc, SocketPath(e.paths))
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(srv.Path()); !os.IsNotExist(err) {
		t.Errorf("socket still there: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestListenStaleAndLiveSockets(t *testing.T) {
	e := newEnv(t)
	path := SocketPath(e.paths)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A crashed server leaves a socket file with no listener behind.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("stale socket not left behind")
	}
	srv := NewServer(e.svc, path)
	if err := srv.Listen(); err != nil {
		t.Fatalf("stale socket must be replaced: %v", err)
	}
	go srv.Serve()
	defer srv.Close()

	other := NewServer(e.svc, path)
	if err := other.Listen(); err == nil || !strings.Contains(err.Error(), "another fleet server") {
		t.Errorf("live socket: %v", err)
	}
	// The live server must not have been disturbed.
	c, err := Dial(bg, path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ping(bg); err != nil {
		t.Errorf("ping after failed second listen: %v", err)
	}
}

func TestListenRefusesRegularFileAndLongPath(t *testing.T) {
	e := newEnv(t)
	path := SocketPath(e.paths)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewServer(e.svc, path).Listen(); err == nil {
		t.Error("clobbered a regular file")
	}
	if b, _ := os.ReadFile(path); string(b) != "precious" {
		t.Error("file was modified")
	}
	long := filepath.Join(os.TempDir(), strings.Repeat("x", 120), "fleet.sock")
	if err := NewServer(e.svc, long).Listen(); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("long path: %v", err)
	}
	if err := NewServer(e.svc, path).Serve(); err == nil {
		t.Error("Serve before Listen must fail")
	}
}

func TestRPCLifecycleOverSocket(t *testing.T) {
	e := newEnv(t)
	_, c := startServer(t, e)
	if err := c.Subscribe(bg); err != nil {
		t.Fatal(err)
	}

	task, err := c.Delegate(bg, DelegateParams{Space: "alpha", Task: "rpc-1", Brief: "Do it", Criteria: []string{"works"}, JiraKey: "PROJ-3"})
	if err != nil || task.Status != schema.StatusQueued {
		t.Fatalf("delegate: %+v %v", task, err)
	}
	if task, err = c.Spawn(bg, "rpc-1", ""); err != nil || task.Status != schema.StatusWorking {
		t.Fatalf("spawn: %+v %v", task, err)
	}
	waitFor(t, "review", func() bool {
		ts, err := c.Tasks(bg, TaskFilter{Status: schema.StatusReview})
		return err == nil && len(ts) == 1
	})
	e.waitIdle("rpc-1")
	res, err := c.Review(bg, "rpc-1")
	if err != nil || res.Verdict.Verdict != schema.DecisionApproved {
		t.Fatalf("review: %+v %v", res, err)
	}
	e.gh.mu.Lock()
	e.gh.status = &integrations.PRStatus{
		PullRequest: integrations.PullRequest{Number: 5, State: "open", HeadSHA: res.SHA, Mergeable: ptr(true)},
		Checks:      integrations.ChecksNone,
	}
	e.gh.mu.Unlock()
	merged, err := c.Merge(bg, "rpc-1")
	if err != nil || merged.Status != schema.StatusDone {
		t.Fatalf("merge: %+v %v", merged, err)
	}

	var rec TaskRecord
	if err := c.Call(bg, "tasks.get", TaskParams{Task: "rpc-1"}, &rec); err != nil || len(rec.Reviews) != 1 || rec.Task.SpendUSD <= 0 {
		t.Errorf("tasks.get: %+v %v", rec, err)
	}

	// Pushed events arrive in order and are strictly valid.
	var got []schema.EventType
	timeout := time.After(5 * time.Second)
	for len(got) < 6 {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatal("event stream closed")
			}
			if err := ev.Validate(); err != nil {
				t.Errorf("invalid pushed event: %v", err)
			}
			got = append(got, ev.Type)
		case <-timeout:
			t.Fatalf("events so far: %v", got)
		}
	}
	if got[len(got)-1] != schema.EventTaskCompleted || got[0] != schema.EventTaskProgress {
		t.Errorf("events = %v", got)
	}

	var evs []schema.AgentEvent
	if err := c.Call(bg, "events.list", EventsParams{Limit: 3}, &evs); err != nil || len(evs) != 3 {
		t.Errorf("events.list: %d %v", len(evs), err)
	}
	var cost CostReport
	if err := c.Call(bg, "cost", CostParams{Space: "alpha"}, &cost); err != nil || cost.Usage.Cost.Total < 0.7 {
		t.Errorf("cost: %+v %v", cost, err)
	}
}

func TestRPCSpacesAndBudgets(t *testing.T) {
	e := newEnv(t)
	_, c := startServer(t, e)
	add := AddSpaceParams{Space: config.Space{ID: "gamma", Projects: []config.Project{{ID: "p", Path: e.repo}}}}
	if err := c.Call(bg, "spaces.add", add, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(bg, "spaces.add", add, nil); !errors.Is(err, ErrSpaceExists) {
		t.Errorf("duplicate: %v", err)
	}
	if err := c.Call(bg, "spaces.add_project", AddProjectParams{Space: "gamma", Project: config.Project{ID: "q", Path: e.repo}}, nil); err != nil {
		t.Fatal(err)
	}
	b := 2.5
	if err := c.Call(bg, "spaces.budget", SpaceBudgetParams{Space: "gamma", BudgetUpdate: BudgetUpdate{MaxUSD: &b}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(bg, "fleet.budget", FleetBudgetParams{MaxUSD: 9}, nil); err != nil {
		t.Fatal(err)
	}
	sp, err := c.Spaces(bg)
	if err != nil || len(sp) != 2 || sp[1].ID != "gamma" || sp[1].MaxBudgetUSD != 2.5 || len(sp[1].Projects) != 2 {
		t.Fatalf("spaces = %+v %v", sp, err)
	}
	if err := c.Call(bg, "spaces.remove", RemoveSpaceParams{ID: "gamma"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(bg, "spaces.remove", RemoveSpaceParams{ID: "gamma"}, nil); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("remove twice: %v", err)
	}
	if err := c.Call(bg, "fleet.budget", FleetBudgetParams{MaxUSD: -1}, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("negative budget: %v", err)
	}
}

func TestRPCErrorsKeepSentinels(t *testing.T) {
	e := newEnv(t)
	_, c := startServer(t, e)
	var re *RPCError

	_, err := c.Spawn(bg, "ghost", "")
	if !errors.Is(err, ErrNoSuchTask) || !errors.As(err, &re) || re.Kind != KindNotFound || re.Code != codeApplication {
		t.Errorf("ghost: %#v", err)
	}
	if errors.Is(err, ErrBusy) {
		t.Error("sentinel matching too broad")
	}
	if _, err := c.Delegate(bg, DelegateParams{Space: "alpha", Task: "t1", Brief: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Delegate(bg, DelegateParams{Space: "alpha", Task: "t1", Brief: "x"}); !errors.Is(err, ErrTaskExists) {
		t.Errorf("dup: %v", err)
	}
	if _, err := c.Merge(bg, "t1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("merge queued: %v", err)
	}
	if _, err := c.Report(bg, "t1", "progress", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("empty note: %v", err)
	}
	if err := c.Call(bg, "spaces.budget", SpaceBudgetParams{Space: "alpha", BudgetUpdate: BudgetUpdate{MaxUSD: ptr(0.0)}}, nil); err != nil {
		t.Fatal(err)
	}
	e.runner.workerFn = func(context.Context, driver.WorkerRequest) (*driver.WorkerResult, error) { return &driver.WorkerResult{}, nil }
	// Budget errors cross the wire too.
	if err := c.Call(bg, "spaces.budget", SpaceBudgetParams{Space: "alpha", BudgetUpdate: BudgetUpdate{MaxUSD: ptr(0.01)}}, nil); err != nil {
		t.Fatal(err)
	}
	e.svc.tracker.Record("alpha", "t1", "worker", &budget.SessionStats{ID: "s", Total: budget.Usage{Cost: budget.Cost{Total: 1}}})
	if _, err := c.Spawn(bg, "t1", ""); !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Errorf("budget: %v", err)
	}
}

// rawExchange sends lines on one connection and returns n responses keyed by
// their id (rendered as JSON; "null" for a null id). Requests on a connection
// are served concurrently, so responses may arrive in any order.
func rawExchange(t *testing.T, path string, n int, lines ...string) map[string]map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for _, l := range lines {
		if _, err := fmt.Fprintln(conn, l); err != nil {
			t.Fatal(err)
		}
	}
	br := bufio.NewReader(conn)
	out := map[string]map[string]any{}
	for i := 0; i < n; i++ {
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v (got %v)", err, out)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		id, _ := json.Marshal(m["id"])
		out[string(id)] = m
	}
	return out
}

func rpcCode(m map[string]any) float64 {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(float64)
	}
	return 0
}

func TestProtocolErrors(t *testing.T) {
	e := newEnv(t)
	srv, _ := startServer(t, e)
	out := rawExchange(t, srv.Path(), 6,
		`{not json`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"nope"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tasks.get","params":{"task":"x","extra":1}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tasks.get","params":[1]}`,
		`{"jsonrpc":"2.0","id":"str-id","method":"ping"}`,
	)
	want := map[string]float64{
		"null": codeParse, "1": codeInvalidRequest, "2": codeNoMethod, "3": codeInvalidParams, "4": codeInvalidParams, `"str-id"`: 0,
	}
	for id, w := range want {
		m, ok := out[id]
		if !ok {
			t.Errorf("no response for id %s", id)
			continue
		}
		if got := rpcCode(m); got != w {
			t.Errorf("id %s: code = %v, want %v (%v)", id, got, w, m)
		}
	}
	if out[`"str-id"`]["result"] == nil {
		t.Errorf("string id request got no result: %v", out[`"str-id"`])
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	e := newEnv(t)
	srv, _ := startServer(t, e)
	conn, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// A notification (no id) is executed but not answered, not even on error.
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","method":"tasks.delegate","params":{"space":"alpha","task":"n1","brief":"x"}}`)
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","method":"tasks.get","params":{"task":"missing"}}`)
	waitFor(t, "notification to run", func() bool { _, err := e.svc.GetTask("n1"); return err == nil })
	fmt.Fprintln(conn, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(line, &m)
	if m["id"] != float64(9) || m["error"] != nil {
		t.Errorf("first response must be the ping: %s", line)
	}
}

func TestOversizedRequestClosesConnection(t *testing.T) {
	e := newEnv(t)
	srv, c := startServer(t, e)
	conn, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	go func() {
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 6; i++ {
			if _, err := conn.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil || !strings.Contains(string(line), "too large") {
		t.Errorf("response = %q, %v", line, err)
	}
	// Other connections are unaffected.
	if err := c.Ping(bg); err != nil {
		t.Errorf("ping: %v", err)
	}
}

func TestConcurrentClients(t *testing.T) {
	e := newEnv(t)
	srv, _ := startServer(t, e)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := Dial(bg, srv.Path())
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			for j := 0; j < 5; j++ {
				id := fmt.Sprintf("c%d-%d", i, j)
				if _, err := c.Delegate(bg, DelegateParams{Space: "alpha", Task: id, Brief: "x"}); err != nil {
					errs <- err
				}
				if err := c.Ping(bg); err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(e.svc.ListTasks(TaskFilter{})); n != 40 {
		t.Errorf("tasks = %d, want 40", n)
	}
}

func TestClientSeesServerShutdown(t *testing.T) {
	e := newEnv(t)
	srv := NewServer(e.svc, SocketPath(e.paths))
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	c, err := Dial(bg, srv.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ping(bg); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client did not notice shutdown")
	}
	if err := c.Ping(bg); !errors.Is(err, ErrClientClosed) {
		t.Errorf("call after shutdown: %v", err)
	}
	if _, err := Dial(bg, srv.Path()); err == nil {
		t.Error("dial after shutdown succeeded")
	}
}

func TestClientContextCancel(t *testing.T) {
	e := newEnv(t)
	_, c := startServer(t, e)
	block := make(chan struct{})
	e.runner.reviewFn = func(ctx context.Context, _ driver.ReviewerRequest) (*driver.ReviewerResult, error) {
		<-block
		return nil, errors.New("unblocked")
	}
	defer close(block)
	e.toReview("t1")
	ctx, cancel := context.WithTimeout(bg, 100*time.Millisecond)
	defer cancel()
	if _, err := c.Review(ctx, "t1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	// The connection stays usable.
	if err := c.Ping(bg); err != nil {
		t.Errorf("ping: %v", err)
	}
}
