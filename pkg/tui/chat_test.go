package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type chatFn func(ctx context.Context, spaceID, text string) (string, error)

func (f chatFn) Chat(ctx context.Context, s, t string) (string, error) { return f(ctx, s, t) }

func (f chatFn) ChatHistory(context.Context, string) ([]ChatMessage, error) { return nil, nil }

func newChat(b ChatBackend) *ChatModel {
	c := NewChat(context.Background(), NewStyles(), b)
	c.SetSize(60, 14)
	c.SetSpace("s1", "alpha")
	c.Focus()
	return c
}

// pump runs cmd and feeds the resulting messages back into the chat until idle.
func pump(t *testing.T, c *ChatModel, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0 && steps < 50; steps++ {
		cm := queue[0]
		queue = queue[1:]
		if cm == nil {
			continue
		}
		msg, ok := runCmd(cm, 200*time.Millisecond)
		if !ok {
			continue
		}
		switch v := msg.(type) {
		case tea.BatchMsg:
			queue = append(queue, v...)
		case ChatReplyMsg:
			queue = append(queue, c.Update(v))
		case ChatHistoryMsg:
			queue = append(queue, c.Update(v))
		}
	}
}

func typeInto(c *ChatModel, s string) {
	for _, r := range s {
		c.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestChatSendAndReply(t *testing.T) {
	c := newChat(chatFn(func(_ context.Context, id, text string) (string, error) {
		return "pong:" + id + ":" + text, nil
	}))
	typeInto(c, "ping")
	cmd := c.Update(keyMsg("enter"))
	if c.Value() != "" {
		t.Errorf("input not cleared: %q", c.Value())
	}
	if !c.Pending() {
		t.Error("expected pending after send")
	}
	pump(t, c, cmd)
	msgs := c.Messages()
	if len(msgs) != 2 || msgs[0].Role != ChatUser || msgs[1].Role != ChatLead || msgs[1].Text != "pong:s1:ping" {
		t.Fatalf("messages = %+v", msgs)
	}
	if c.Pending() {
		t.Error("still pending after reply")
	}
	if v := ansi.Strip(c.View()); !strings.Contains(v, "pong:s1:ping") || !strings.Contains(v, "you") || !strings.Contains(v, "lead") {
		t.Errorf("transcript not rendered:\n%s", v)
	}
}

func TestChatMultilineKeys(t *testing.T) {
	for _, nl := range []string{"ctrl+j", "alt+enter"} {
		c := newChat(chatFn(func(context.Context, string, string) (string, error) { return "ok", nil }))
		typeInto(c, "one")
		c.Update(keyMsg(nl))
		typeInto(c, "two")
		if c.Value() != "one\ntwo" {
			t.Errorf("%s: value = %q", nl, c.Value())
		}
		if len(c.Messages()) != 0 {
			t.Errorf("%s must not send", nl)
		}
		if c.inputHeight() != 2 {
			t.Errorf("%s: input height = %d, want 2", nl, c.inputHeight())
		}
		cmd := c.Update(keyMsg("enter"))
		pump(t, c, cmd)
		if m := c.Messages(); len(m) == 0 || m[0].Text != "one\ntwo" {
			t.Errorf("%s: sent %+v", nl, m)
		}
	}
}

func TestChatInputGrowthIsCapped(t *testing.T) {
	c := newChat(nil)
	for i := 0; i < 20; i++ {
		typeInto(c, "x")
		c.Update(keyMsg("ctrl+j"))
	}
	if h := c.inputHeight(); h != maxInputLines {
		t.Errorf("input height = %d, want %d", h, maxInputLines)
	}
	assertBox(t, c.View(), 60, 14)
}

func TestChatRapidPingPongKeepsOrder(t *testing.T) {
	var got []string
	release := make(chan struct{})
	c := newChat(chatFn(func(_ context.Context, _, text string) (string, error) {
		got = append(got, text)
		<-release
		return "re:" + text, nil
	}))
	typeInto(c, "a")
	cmd1 := c.Update(keyMsg("enter"))
	typeInto(c, "b")
	cmd2 := c.Update(keyMsg("enter"))
	typeInto(c, "c")
	cmd3 := c.Update(keyMsg("enter"))
	if cmd2 != nil || cmd3 != nil {
		t.Error("messages typed while pending must be queued, not dispatched")
	}
	if c.Queued() != 2 {
		t.Fatalf("queued = %d", c.Queued())
	}
	if v := ansi.Strip(c.View()); !strings.Contains(v, "2 queued") {
		t.Errorf("queue indicator missing:\n%s", v)
	}
	go func() {
		for i := 0; i < 3; i++ {
			release <- struct{}{}
		}
	}()
	pump(t, c, cmd1)
	var texts []string
	for _, m := range c.Messages() {
		texts = append(texts, m.Text)
	}
	want := "a,b,c,re:a,re:b,re:c"
	// user messages are shown immediately; replies follow in send order
	if strings.Join(texts, ",") != want {
		t.Errorf("transcript = %v, want %s", texts, want)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("backend order = %v", got)
	}
}

func TestChatErrorAndEmptyInput(t *testing.T) {
	c := newChat(chatFn(func(context.Context, string, string) (string, error) { return "", errors.New("lead crashed") }))
	if cmd := c.Update(keyMsg("enter")); cmd != nil || len(c.Messages()) != 0 {
		t.Error("empty input must not send")
	}
	typeInto(c, "   ")
	if cmd := c.Update(keyMsg("enter")); cmd != nil || len(c.Messages()) != 0 {
		t.Error("blank input must not send")
	}
	typeInto(c, "hi")
	pump(t, c, c.Update(keyMsg("enter")))
	m := c.Messages()
	if len(m) != 2 || m[1].Role != ChatSystem || !strings.Contains(m[1].Text, "lead crashed") {
		t.Errorf("messages = %+v", m)
	}
	if c.Pending() {
		t.Error("pending must clear after an error")
	}
}

func TestChatNoBackend(t *testing.T) {
	c := newChat(nil)
	typeInto(c, "hello")
	if cmd := c.Update(keyMsg("enter")); cmd != nil {
		t.Error("no backend: expected no command")
	}
	m := c.Messages()
	if len(m) != 2 || m[1].Role != ChatSystem || !strings.Contains(m[1].Text, "no backend") {
		t.Errorf("messages = %+v", m)
	}
}

func TestChatPerSpaceTranscriptAndDraft(t *testing.T) {
	c := newChat(chatFn(func(context.Context, string, string) (string, error) { return "ok", nil }))
	pump(t, c, func() tea.Cmd { typeInto(c, "one"); return c.Update(keyMsg("enter")) }())
	typeInto(c, "draft-a")
	c.SetSpace("s2", "beta")
	if len(c.Messages()) != 0 || c.Value() != "" {
		t.Errorf("s2 should be empty: %+v %q", c.Messages(), c.Value())
	}
	typeInto(c, "draft-b")
	c.SetSpace("s1", "alpha")
	if len(c.Messages()) != 2 || c.Value() != "draft-a" {
		t.Errorf("s1 not restored: %d msgs, %q", len(c.Messages()), c.Value())
	}
	c.SetSpace("s2", "beta")
	if c.Value() != "draft-b" {
		t.Errorf("s2 draft = %q", c.Value())
	}
}

func TestChatReplyForOtherSpaceDoesNotLeak(t *testing.T) {
	c := newChat(nil)
	c.Update(ChatReplyMsg{SpaceID: "s9", Reply: "hello"})
	if len(c.Messages()) != 0 {
		t.Error("reply for another space leaked into the active transcript")
	}
	c.SetSpace("s9", "ninth")
	if m := c.Messages(); len(m) != 1 || m[0].Text != "hello" {
		t.Errorf("s9 messages = %+v", m)
	}
}

func TestChatWrapsAndSanitizes(t *testing.T) {
	c := newChat(nil)
	c.Update(ChatReplyMsg{SpaceID: "s1", Reply: "\x1b[31m" + strings.Repeat("word ", 40) + "\x1b[0m"})
	assertBox(t, c.View(), 60, 14)
	if strings.Contains(c.View(), "\x1b[31m") {
		t.Error("untrusted escape sequence reached the screen")
	}
}

type histBackend struct {
	chatFn
	msgs []ChatMessage
	err  error
}

func (h histBackend) ChatHistory(context.Context, string) ([]ChatMessage, error) {
	return h.msgs, h.err
}

func TestChatLoadHistory(t *testing.T) {
	at := time.Date(2025, 1, 2, 3, 4, 0, 0, time.UTC)
	b := histBackend{
		chatFn: func(context.Context, string, string) (string, error) { return "r", nil },
		msgs: []ChatMessage{
			{Role: ChatUser, Text: "old question", At: at},
			{Role: ChatLead, Text: "old answer", At: at},
		},
	}
	c := newChat(b)
	if !c.NeedsHistory() {
		t.Fatal("fresh space should need history")
	}
	cmd := c.LoadHistory("s1")
	if c.NeedsHistory() {
		t.Error("in-flight load must not be requested twice")
	}
	pump(t, c, cmd)
	if got := c.Messages(); len(got) != 2 || got[1].Text != "old answer" || got[0].Role != ChatUser {
		t.Fatalf("messages = %+v", got)
	}
	if c.NeedsHistory() {
		t.Error("loaded space must not reload")
	}
	if !c.vp.AtBottom() {
		t.Error("viewport should be scrolled to the bottom")
	}
	if v := ansi.Strip(c.View()); !strings.Contains(v, "old answer") {
		t.Errorf("history not rendered:\n%s", v)
	}
}

func TestChatLoadHistoryKeepsMessagesTypedMeanwhile(t *testing.T) {
	b := histBackend{
		chatFn: func(context.Context, string, string) (string, error) { return "r", nil },
		msgs:   []ChatMessage{{Role: ChatLead, Text: "persisted"}},
	}
	c := newChat(b)
	cmd := c.LoadHistory("s1")
	c.SendMessage("typed while loading") // not pumped: stays pending
	pump(t, c, cmd)
	got := c.Messages()
	if len(got) != 2 || got[0].Text != "persisted" || got[1].Text != "typed while loading" {
		t.Fatalf("messages = %+v", got)
	}
}

func TestChatLoadHistoryErrorRetries(t *testing.T) {
	c := newChat(histBackend{err: errors.New("boom")})
	pump(t, c, c.LoadHistory("s1"))
	if len(c.Messages()) != 0 {
		t.Errorf("messages = %+v", c.Messages())
	}
	if !c.NeedsHistory() {
		t.Error("failed load should be retried")
	}
}

func TestChatLoadHistoryNilBackend(t *testing.T) {
	c := newChat(nil)
	if c.LoadHistory("s1") != nil || c.NeedsHistory() {
		t.Error("nil backend has no history")
	}
}
