package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/pkg/config"
	"fleet/pkg/driver"
	"fleet/pkg/tui"
)

type fakeLead struct {
	done    chan struct{}
	stopped sync.Once
	answer  func(prompt string) (string, error)

	mu      sync.Mutex
	prompts []string
	aborts  int
}

func newFakeLead() *fakeLead {
	return &fakeLead{done: make(chan struct{})}
}

func (f *fakeLead) Prompt(ctx context.Context, msg string) (string, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, msg)
	ans := f.answer
	f.mu.Unlock()
	if ans != nil {
		replyCh := make(chan struct {
			r string
			e error
		}, 1)
		go func() {
			r, e := ans(msg)
			replyCh <- struct {
				r string
				e error
			}{r, e}
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case res := <-replyCh:
			return res.r, res.e
		}
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
		return "re: " + msg, nil
	}
}

func (f *fakeLead) Done() <-chan struct{} { return f.done }
func (f *fakeLead) Abort() error          { f.mu.Lock(); f.aborts++; f.mu.Unlock(); return nil }
func (f *fakeLead) Stop(time.Duration) error {
	f.stopped.Do(func() { close(f.done) })
	return nil
}

func (f *fakeLead) isStopped() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

type leadStarter struct {
	mu     sync.Mutex
	leads  []*fakeLead
	reqs   []driver.LeadRequest
	answer func(prompt string) (string, error)
	err    error
}

func (ls *leadStarter) start(_ context.Context, req driver.LeadRequest) (driver.LeadSession, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.err != nil {
		return nil, ls.err
	}
	f := newFakeLead()
	f.answer = ls.answer
	ls.leads = append(ls.leads, f)
	ls.reqs = append(ls.reqs, req)
	return f, nil
}

func (ls *leadStarter) count() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return len(ls.leads)
}

func TestChatStartsLeadOnceAndSkipsIntro(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start

	if st := e.svc.LeadStatuses(); st["alpha"] != LeadOffline {
		t.Fatalf("before chat: %v", st)
	}
	reply, err := e.svc.Chat(bg, "alpha", "  hello  ")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "re: hello" {
		t.Errorf("reply = %q", reply)
	}
	if reply, err = e.svc.Chat(bg, "alpha", "again"); err != nil || reply != "re: again" {
		t.Errorf("second reply = %q, %v", reply, err)
	}
	if ls.count() != 1 {
		t.Errorf("lead started %d times, want 1", ls.count())
	}
	if st := e.svc.LeadStatuses(); st["alpha"] != LeadIdle {
		t.Errorf("after chat: %v", st)
	}
	req := ls.reqs[0]
	if req.Space != "alpha" || req.Dir != e.repo {
		t.Errorf("lead options = %+v", req)
	}
	if got := ls.leads[0].prompts; len(got) != 2 || got[0] != "hello" {
		t.Errorf("prompts = %q", got)
	}
}

func TestChatErrors(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start
	if _, err := e.svc.Chat(bg, "alpha", "   "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("empty message: %v", err)
	}
	if _, err := e.svc.Chat(bg, "nope", "hi"); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("unknown space: %v", err)
	}
	ls.err = errors.New("no pi")
	if _, err := e.svc.Chat(bg, "alpha", "hi"); err == nil || !strings.Contains(err.Error(), "no pi") {
		t.Errorf("start failure: %v", err)
	}
	if st := e.svc.LeadStatuses(); st["alpha"] != LeadOffline {
		t.Errorf("status after failed start: %v", st)
	}
}

func TestChatWorkingStateSerialisesAndCancelRestarts(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	hold := make(chan struct{})
	ls.answer = func(prompt string) (string, error) {
		if prompt == "slow" {
			<-hold
		}
		return "re: " + prompt, nil
	}
	e.runner.leadFn = ls.start

	ctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	go func() { _, err := e.svc.Chat(ctx, "alpha", "slow"); errc <- err }()
	waitFor(t, "lead working", func() bool { return e.svc.LeadStatuses()["alpha"] == LeadWorking })

	short, cancelShort := context.WithTimeout(bg, 50*time.Millisecond)
	if _, err := e.svc.Chat(short, "alpha", "queued"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued chat: %v", err)
	}
	cancelShort()

	cancel()
	close(hold)
	if err := <-errc; err == nil {
		t.Error("cancelled chat returned no error")
	}
	waitFor(t, "lead dropped", func() bool { return e.svc.LeadStatuses()["alpha"] == LeadOffline })
	if !ls.leads[0].isStopped() {
		t.Error("cancelled lead was not stopped")
	}
	reply, err := e.svc.Chat(bg, "alpha", "after")
	if err != nil || reply != "re: after" {
		t.Errorf("chat after cancel = %q, %v", reply, err)
	}
	if ls.count() != 2 {
		t.Errorf("lead starts = %d, want 2", ls.count())
	}
}

func TestChatLeadExitAndRestart(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start
	if _, err := e.svc.Chat(bg, "alpha", "one"); err != nil {
		t.Fatal(err)
	}
	_ = ls.leads[0].Stop(0) // process dies between messages
	if reply, err := e.svc.Chat(bg, "alpha", "two"); err != nil || reply != "re: two" {
		t.Errorf("after exit: %q, %v", reply, err)
	}
	if ls.count() != 2 {
		t.Errorf("lead starts = %d, want 2", ls.count())
	}

	// Dying mid-answer is an error, not a hang.
	ls.answer = func(string) (string, error) {
		_ = ls.leads[1].Stop(0)
		return "", errors.New("lead process exited")
	}
	ls.leads[1].answer = ls.answer
	if _, err := e.svc.Chat(bg, "alpha", "three"); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("mid-answer exit: %v", err)
	}
}

func TestChatRPCAndShutdown(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start
	_, c := startServer(t, e)

	reply, err := c.Chat(bg, "alpha", "over the wire")
	if err != nil || reply != "re: over the wire" {
		t.Fatalf("rpc chat = %q, %v", reply, err)
	}
	st, err := c.LeadStatuses(bg)
	if err != nil || st["alpha"] != LeadIdle {
		t.Errorf("lead statuses = %v, %v", st, err)
	}
	if _, err := c.Chat(bg, "ghost", "hi"); !errors.Is(err, ErrNoSuchSpace) {
		t.Errorf("rpc unknown space: %v", err)
	}

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := e.svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if !ls.leads[0].isStopped() {
		t.Error("shutdown left the lead running")
	}
	if _, err := e.svc.Chat(bg, "alpha", "late"); !errors.Is(err, ErrShutdown) {
		t.Errorf("chat after shutdown: %v", err)
	}
}

func TestChatPersistsConversation(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start

	if h, err := e.svc.ChatHistory("alpha"); err != nil || h == nil || len(h) != 0 {
		t.Fatalf("empty history = %#v, %v", h, err)
	}
	before := time.Now().Add(-time.Second)
	if _, err := e.svc.Chat(bg, "alpha", " hello "); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Chat(bg, "alpha", "again"); err != nil {
		t.Fatal(err)
	}
	h, err := e.svc.ChatHistory("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 4 {
		t.Fatalf("messages = %d, want 4 (2 user + 2 lead)", len(h))
	}
	wantRoles := []string{RoleUser, RoleLead, RoleUser, RoleLead}
	wantTexts := []string{"hello", "re: hello", "again", "re: again"}
	for i, m := range h {
		if m.Role != wantRoles[i] || m.Text != wantTexts[i] {
			t.Errorf("m[%d] = %+v, want role %q text %q", i, m, wantRoles[i], wantTexts[i])
		}
		if m.At.Before(before) || m.At.After(time.Now().Add(time.Second)) {
			t.Errorf("m[%d].At suspicious: %v", i, m.At)
		}
	}

	// Verify chat.json permissions and format.
	dir := filepath.Join(e.paths.DataDir, "spaces", "alpha")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ent := range ents {
		if ent.Name() == "chat.json" {
			found = true
		}
	}
	if !found {
		t.Errorf("chat.json missing in %s: %v", dir, ents)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "chat.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("chat.json mode = %v", fi.Mode())
	}
}

func TestChatHistoryKeepsFailuresAndIsPerSpace(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{err: errors.New("no pi")}
	e.runner.leadFn = ls.start
	if _, err := e.svc.Chat(bg, "alpha", "hi"); err == nil {
		t.Fatal("expected start failure")
	}
	h, err := e.svc.ChatHistory("alpha")
	if err != nil || len(h) != 2 {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if h[0].Role != RoleUser || h[0].Text != "hi" || h[1].Role != RoleSystem || !strings.Contains(h[1].Text, "no pi") {
		t.Errorf("history = %+v", h)
	}

	if err := e.svc.AddSpace(config.Space{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	ls.err = nil
	if _, err := e.svc.Chat(bg, "beta", "hello beta"); err != nil {
		t.Fatal(err)
	}
	hb, err := e.svc.ChatHistory("beta")
	if err != nil || len(hb) != 2 {
		t.Fatalf("beta history = %+v, %v", hb, err)
	}
	if hb[0].Text != "hello beta" || hb[1].Text != "re: hello beta" {
		t.Errorf("beta history = %+v", hb)
	}
	hAlpha, _ := e.svc.ChatHistory("alpha")
	if len(hAlpha) != 2 {
		t.Errorf("alpha mutated: %+v", hAlpha)
	}
}

func TestChatHistoryCorruptFile(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.paths.DataDir, "spaces", "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat.json"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChatHistory("alpha"); err == nil || !strings.Contains(err.Error(), "chat.json") {
		t.Errorf("corrupt history: %v", err)
	}
}

func TestChatHistoryRPCAndAdapter(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.runner.leadFn = ls.start
	ad := adapters(t, e)
	for name, a := range ad {
		if h, err := a.ChatHistory(bg, "alpha"); err != nil || len(h) != 0 {
			t.Fatalf("%s: empty rpc history = %v, %v", name, h, err)
		}
	}
	if _, err := ad["client"].Chat(bg, "alpha", "over the wire"); err != nil {
		t.Fatal(err)
	}
	for name, a := range ad {
		h, err := a.ChatHistory(bg, "alpha")
		if err != nil || len(h) != 2 || h[0].Role != tui.ChatUser || h[1].Text != "re: over the wire" {
			t.Fatalf("%s: history = %+v, %v", name, h, err)
		}
	}
}
