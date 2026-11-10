package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"fleet/pkg/pi"
)

// fakeLead stands in for a headless Pi lead process.
type fakeLead struct {
	events  chan pi.Event
	done    chan struct{}
	stopped sync.Once
	// answer produces the events for one prompt; nil replies "re: <prompt>".
	answer func(prompt string) []pi.Event

	mu      sync.Mutex
	prompts []string
	aborts  int
}

func newFakeLead() *fakeLead {
	return &fakeLead{events: make(chan pi.Event, 64), done: make(chan struct{})}
}

func assistantEvent(text string) pi.Event {
	raw, _ := json.Marshal(map[string]any{
		"type":    "message_end",
		"message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": text}}},
	})
	return pi.Event{Type: "message_end", Raw: raw}
}

func settled() pi.Event {
	return pi.Event{Type: "agent_settled", Raw: json.RawMessage(`{"type":"agent_settled","aborted":false}`)}
}

func (f *fakeLead) Prompt(msg string) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, msg)
	ans := f.answer
	f.mu.Unlock()
	evs := []pi.Event{assistantEvent("re: " + msg), settled()}
	if ans != nil {
		evs = ans(msg)
	}
	go func() {
		for _, e := range evs {
			select {
			case f.events <- e:
			case <-f.done:
				return
			}
		}
	}()
	return nil
}

func (f *fakeLead) Events() <-chan pi.Event  { return f.events }
func (f *fakeLead) Done() <-chan struct{}    { return f.done }
func (f *fakeLead) Abort() error             { f.mu.Lock(); f.aborts++; f.mu.Unlock(); return nil }
func (f *fakeLead) Stop(time.Duration) error { f.stopped.Do(func() { close(f.done) }); return nil }
func (f *fakeLead) isStopped() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// leadStarter returns a starter that hands out fake leads (each already
// queued with an intro turn) and records the options it was given.
type leadStarter struct {
	mu     sync.Mutex
	leads  []*fakeLead
	opts   []pi.LeadOptions
	answer func(prompt string) []pi.Event
	err    error
}

func (ls *leadStarter) start(_ context.Context, o pi.LeadOptions) (LeadProcess, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.err != nil {
		return nil, ls.err
	}
	f := newFakeLead()
	f.answer = ls.answer
	f.events <- assistantEvent("intro: ready")
	f.events <- settled()
	ls.leads = append(ls.leads, f)
	ls.opts = append(ls.opts, o)
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
	e.svc.opts.StartLead = ls.start

	if st := e.svc.LeadStatuses(); st["alpha"] != LeadOffline {
		t.Fatalf("before chat: %v", st)
	}
	reply, err := e.svc.Chat(bg, "alpha", "  hello  ")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "re: hello" {
		t.Errorf("reply = %q (intro turn leaked?)", reply)
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
	o := ls.opts[0]
	if o.Space != "alpha" || o.Dir != e.repo {
		t.Errorf("lead options = %+v", o)
	}
	if got := ls.leads[0].prompts; len(got) != 2 || got[0] != "hello" {
		t.Errorf("prompts = %q", got)
	}
}

func TestChatErrors(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.svc.opts.StartLead = ls.start
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
	ls.answer = func(prompt string) []pi.Event {
		if prompt == "slow" {
			<-hold // block this prompt's reply (runs in the fake's goroutine)
		}
		return []pi.Event{assistantEvent("re: " + prompt), settled()}
	}
	e.svc.opts.StartLead = ls.start

	// The fake calls answer synchronously inside Prompt, so "slow" blocks Prompt itself.
	ctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	go func() { _, err := e.svc.Chat(ctx, "alpha", "slow"); errc <- err }()
	waitFor(t, "lead working", func() bool { return e.svc.LeadStatuses()["alpha"] == LeadWorking })

	// A second message waits for the first and honours its own context.
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
	e.svc.opts.StartLead = ls.start
	if _, err := e.svc.Chat(bg, "alpha", "one"); err != nil {
		t.Fatal(err)
	}
	_ = ls.leads[0].Stop(0) // the process dies between messages
	if reply, err := e.svc.Chat(bg, "alpha", "two"); err != nil || reply != "re: two" {
		t.Errorf("after exit: %q, %v", reply, err)
	}
	if ls.count() != 2 {
		t.Errorf("lead starts = %d, want 2", ls.count())
	}

	// Dying mid-answer is an error, not a hang.
	ls.answer = func(string) []pi.Event { go func() { _ = ls.leads[1].Stop(0) }(); return nil }
	ls.leads[1].answer = ls.answer
	if _, err := e.svc.Chat(bg, "alpha", "three"); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("mid-answer exit: %v", err)
	}
}

func TestChatRPCAndShutdown(t *testing.T) {
	e := newEnv(t)
	ls := &leadStarter{}
	e.svc.opts.StartLead = ls.start
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

func TestAssistantMessage(t *testing.T) {
	cases := []struct {
		name, raw, text, errText string
		ok                       bool
	}{
		{"blocks", `{"message":{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"toolCall"},{"type":"text","text":"b"}]}}`, "a\n\nb", "", true},
		{"string", `{"message":{"role":"assistant","content":" hi "}}`, "hi", "", true},
		{"user", `{"message":{"role":"user","content":"x"}}`, "", "", false},
		{"error", `{"message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"boom"}}`, "", "boom", true},
		{"error without message", `{"message":{"role":"assistant","content":[],"stopReason":"error"}}`, "", "assistant run failed", true},
		{"garbage", `nope`, "", "", false},
	}
	for _, c := range cases {
		text, errText, ok := assistantMessage(json.RawMessage(c.raw))
		if text != c.text || errText != c.errText || ok != c.ok {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", c.name, text, errText, ok, c.text, c.errText, c.ok)
		}
	}
}

func TestAwaitSettledFailures(t *testing.T) {
	f := newFakeLead()
	f.events <- assistantEvent("")
	f.events <- pi.Event{Type: "message_end", Raw: json.RawMessage(`{"message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"quota"}}`)}
	f.events <- settled()
	if _, err := awaitSettled(bg, f); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("failed run: %v", err)
	}

	f = newFakeLead()
	f.events <- pi.Event{Type: "response", Raw: json.RawMessage(`{"type":"response","command":"prompt","success":false,"error":"bad"}`)}
	if _, err := awaitSettled(bg, f); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("rejected prompt: %v", err)
	}

	f = newFakeLead()
	f.events <- assistantEvent("partial")
	close(f.events)
	_ = f.Stop(0) // a real process closes its event stream, then reports done
	if _, err := awaitSettled(bg, f); err == nil {
		t.Error("closed event stream without settle returned no error")
	}
}
