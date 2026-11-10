package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// maxInputLines caps how tall the multiline input may grow.
const maxInputLines = 5

// ChatRole identifies who authored a chat message.
type ChatRole int

const (
	ChatUser ChatRole = iota
	ChatLead
	ChatSystem
)

// ChatMessage is one transcript entry.
type ChatMessage struct {
	Role ChatRole
	Text string
	At   time.Time
}

// ChatBackend is the part of Backend the chat pane needs.
type ChatBackend interface {
	Chat(ctx context.Context, spaceID, text string) (string, error)
}

// conversation is the per-space transcript and send queue.
type conversation struct {
	msgs    []ChatMessage
	pending bool     // a request is in flight for this space
	queue   []string // messages typed while pending, sent in order
	draft   string
}

// ChatModel is the live conversation pane with a space's lead agent. The user
// can keep typing while the lead is answering; messages are queued and sent
// one at a time per space so replies always arrive in order.
type ChatModel struct {
	styles  Styles
	backend ChatBackend
	ctx     context.Context
	now     func() time.Time

	input   textarea.Model
	vp      viewport.Model
	spin    spinner.Model
	convs   map[string]*conversation
	spaceID string
	name    string

	width, height int
}

// NewChat returns a chat pane. backend may be nil.
func NewChat(ctx context.Context, st Styles, backend ChatBackend) *ChatModel {
	ta := textarea.New()
	ta.Prompt = "┃ "
	ta.Placeholder = "Message the lead…  (enter: send · ctrl+j / alt+enter: newline)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	// Enter sends. Terminals that report Shift+Enter distinctly are honoured;
	// Ctrl+J and Alt+Enter work everywhere.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j", "alt+enter"))
	ta.FocusedStyle.CursorLine = st.Text
	ta.FocusedStyle.Prompt = st.Prompt
	ta.BlurredStyle.Prompt = st.Subtle
	ta.FocusedStyle.Placeholder = st.Subtle
	ta.BlurredStyle.Placeholder = st.Subtle

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = st.Accent

	vp := viewport.New(0, 0)
	vp.MouseWheelEnabled = true

	if ctx == nil {
		ctx = context.Background()
	}
	return &ChatModel{
		styles: st, backend: backend, ctx: ctx, now: time.Now,
		input: ta, vp: vp, spin: sp, convs: map[string]*conversation{},
	}
}

func (m *ChatModel) conv(id string) *conversation {
	c, ok := m.convs[id]
	if !ok {
		c = &conversation{}
		m.convs[id] = c
	}
	return c
}

// SetSpace switches the active conversation, preserving per-space drafts.
func (m *ChatModel) SetSpace(id, name string) {
	if id == m.spaceID && name == m.name {
		return
	}
	if id != m.spaceID {
		m.conv(m.spaceID).draft = m.input.Value()
		m.spaceID = id
		m.input.SetValue(m.conv(id).draft)
	}
	m.name = name
	m.resize()
	m.refresh(true)
}

// Messages returns a copy of the active conversation.
func (m *ChatModel) Messages() []ChatMessage {
	return append([]ChatMessage(nil), m.conv(m.spaceID).msgs...)
}

// Pending reports whether the active space has a request in flight.
func (m *ChatModel) Pending() bool { return m.conv(m.spaceID).pending }

// Queued returns the number of messages waiting behind the in-flight one.
func (m *ChatModel) Queued() int { return len(m.conv(m.spaceID).queue) }

// Value returns the current draft text.
func (m *ChatModel) Value() string { return m.input.Value() }

// Focus gives the input keyboard focus.
func (m *ChatModel) Focus() tea.Cmd { return m.input.Focus() }

// Blur removes keyboard focus.
func (m *ChatModel) Blur() { m.input.Blur() }

// Focused reports whether the input has focus.
func (m *ChatModel) Focused() bool { return m.input.Focused() }

// SetSize sets the content area (inside the pane frame).
func (m *ChatModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.input.SetWidth(max(1, w))
	m.resize()
	m.refresh(false)
}

// inputHeight grows with the number of lines, within bounds.
func (m *ChatModel) inputHeight() int {
	return clamp(m.input.LineCount(), 1, min(maxInputLines, max(1, m.height-3)))
}

func (m *ChatModel) resize() {
	ih := m.inputHeight()
	m.input.SetHeight(ih)
	m.vp.Width = max(1, m.width)
	m.vp.Height = max(1, m.height-ih-1) // status line
}

// SendMessage submits text as if typed. It returns the command that talks to
// the backend, or nil when the message was queued or rejected.
func (m *ChatModel) SendMessage(text string) tea.Cmd {
	text = strings.TrimSpace(text)
	if text == "" || m.spaceID == "" {
		return nil
	}
	c := m.conv(m.spaceID)
	c.msgs = append(c.msgs, ChatMessage{Role: ChatUser, Text: text, At: m.now()})
	var cmd tea.Cmd
	switch {
	case m.backend == nil:
		c.msgs = append(c.msgs, ChatMessage{Role: ChatSystem, Text: ErrNoBackend.Error(), At: m.now()})
	case c.pending:
		c.queue = append(c.queue, text)
	default:
		cmd = m.dispatch(c, m.spaceID, text)
	}
	m.refresh(true)
	return cmd
}

// dispatch marks the conversation pending and starts the backend call.
func (m *ChatModel) dispatch(c *conversation, spaceID, text string) tea.Cmd {
	c.pending = true
	backend, ctx := m.backend, m.ctx
	call := func() tea.Msg {
		reply, err := backend.Chat(ctx, spaceID, text)
		return ChatReplyMsg{SpaceID: spaceID, Reply: reply, Err: err}
	}
	return tea.Batch(call, m.spin.Tick)
}

func (m *ChatModel) onReply(msg ChatReplyMsg) tea.Cmd {
	c := m.conv(msg.SpaceID)
	switch {
	case msg.Err != nil:
		c.msgs = append(c.msgs, ChatMessage{Role: ChatSystem, Text: "error: " + msg.Err.Error(), At: m.now()})
	case strings.TrimSpace(msg.Reply) != "":
		c.msgs = append(c.msgs, ChatMessage{Role: ChatLead, Text: strings.TrimSpace(msg.Reply), At: m.now()})
	}
	c.pending = false
	var cmd tea.Cmd
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		cmd = m.dispatch(c, msg.SpaceID, next)
	}
	m.refresh(msg.SpaceID == m.spaceID)
	return cmd
}

// Update handles keys, replies and spinner ticks.
func (m *ChatModel) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case ChatReplyMsg:
		return m.onReply(msg)
	case spinner.TickMsg:
		if !m.Pending() {
			return nil // let the tick loop die; dispatch restarts it
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			text := m.input.Value()
			if strings.TrimSpace(text) == "" {
				return nil
			}
			m.input.Reset()
			m.conv(m.spaceID).draft = ""
			cmd := m.SendMessage(text)
			m.resize()
			return cmd
		case "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(rewriteScrollKey(msg))
			return cmd
		}
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.resize()
	return cmd
}

// rewriteScrollKey maps ctrl+u/ctrl+d onto the viewport's half-page keys.
func rewriteScrollKey(k tea.KeyMsg) tea.KeyMsg {
	switch k.String() {
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}}
	}
	return k
}

// refresh rebuilds the transcript; stick scrolls to the bottom afterwards
// (it is also kept when the viewport was already at the bottom).
func (m *ChatModel) refresh(stick bool) {
	if m.width <= 0 {
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(m.renderTranscript())
	if stick || atBottom {
		m.vp.GotoBottom()
	}
}

func (m *ChatModel) renderTranscript() string {
	c := m.conv(m.spaceID)
	if len(c.msgs) == 0 {
		who := m.name
		if who == "" {
			who = "the lead"
		}
		return m.styles.Subtle.Render(wrap("Start a conversation with the lead of "+who+".", m.width))
	}
	var b strings.Builder
	for i, msg := range c.msgs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		label, st := "you", m.styles.ChatUser
		switch msg.Role {
		case ChatLead:
			label, st = "lead", m.styles.ChatLead
		case ChatSystem:
			label, st = "system", m.styles.ChatSystem
		}
		b.WriteString(spread(st.Render(label), m.styles.Subtle.Render(msg.At.Format("15:04")), m.width))
		b.WriteString("\n")
		body := wrap(sanitize(msg.Text), m.width)
		if msg.Role == ChatSystem {
			body = m.styles.ChatSystem.Render(body)
		}
		b.WriteString(body)
	}
	return b.String()
}

func (m *ChatModel) statusLine() string {
	c := m.conv(m.spaceID)
	switch {
	case c.pending && len(c.queue) > 0:
		return m.spin.View() + m.styles.Subtle.Render(fmt.Sprintf(" lead is thinking · %d queued", len(c.queue)))
	case c.pending:
		return m.spin.View() + m.styles.Subtle.Render(" lead is thinking…")
	}
	return m.styles.Subtle.Render(fmt.Sprintf("%d messages · pgup/pgdn scroll", len(c.msgs)))
}

// View renders transcript, status line and input.
func (m *ChatModel) View() string {
	return m.vp.View() + "\n" + truncate(m.statusLine(), m.width) + "\n" + m.input.View()
}
