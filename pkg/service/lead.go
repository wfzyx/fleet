package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fleet/pkg/agent"
	"fleet/pkg/config"
	"fleet/pkg/pi"
)

// Lead states reported by LeadStatuses.
const (
	LeadOffline = "offline" // no lead process
	LeadIdle    = "idle"    // process running, waiting for a message
	LeadWorking = "working" // process is answering a message
)

// leadStartTimeout bounds how long a fresh lead may take to settle its intro turn.
const leadStartTimeout = 2 * time.Minute

// LeadProcess is the part of *pi.Process the lead chat uses.
type LeadProcess interface {
	Prompt(message string) error
	Events() <-chan pi.Event
	Done() <-chan struct{}
	Abort() error
	Stop(grace time.Duration) error
}

// LeadStarter launches a headless lead. The default starts the real Pi
// binary (pi.StartLead), which sends the lead's intro prompt immediately.
type LeadStarter func(ctx context.Context, o pi.LeadOptions) (LeadProcess, error)

func defaultLeadStarter(ctx context.Context, o pi.LeadOptions) (LeadProcess, error) {
	return pi.StartLead(ctx, o)
}

// Chat message roles stored in chat.json.
const (
	RoleUser   = "user"
	RoleLead   = "lead"
	RoleSystem = "system"
)

// ChatMessage is one persisted entry of a space's lead conversation.
type ChatMessage struct {
	Role string    `json:"role"` // "user" | "lead" | "system"
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// chatFile is the on-disk form of chat.json.
type chatFile struct {
	Messages []ChatMessage `json:"messages"`
}

// chatPath is $XDG_DATA_HOME/fleet/spaces/<space>/chat.json.
func (s *Service) chatPath(space string) (string, error) {
	if err := config.ValidateID(space); err != nil {
		return "", fmt.Errorf("%w: space id: %v", ErrInvalidArgument, err)
	}
	return filepath.Join(s.paths.DataDir, "spaces", space, "chat.json"), nil
}

func (s *Service) readChat(space string) ([]ChatMessage, error) {
	path, err := s.chatPath(space)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []ChatMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("service: read chat %s: %w", space, err)
	}
	var f chatFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("service: parse %s: %w", path, err)
	}
	if f.Messages == nil {
		f.Messages = []ChatMessage{}
	}
	return f.Messages, nil
}

// ChatHistory returns the persisted conversation of a space's lead, oldest
// first. A space without conversation yields an empty slice.
func (s *Service) ChatHistory(space string) ([]ChatMessage, error) {
	if _, err := s.GetSpace(space); err != nil {
		return nil, err
	}
	l := s.leadFor(space)
	l.hmu.Lock()
	defer l.hmu.Unlock()
	return s.readChat(space)
}

// recordChat appends msgs to the space's chat.json (atomic write).
func (s *Service) recordChat(space string, msgs ...ChatMessage) error {
	path, err := s.chatPath(space)
	if err != nil {
		return err
	}
	l := s.leadFor(space)
	l.hmu.Lock()
	defer l.hmu.Unlock()
	hist, err := s.readChat(space)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(chatFile{Messages: append(hist, msgs...)}, "", "  ")
	if err != nil {
		return fmt.Errorf("service: encode chat: %w", err)
	}
	if err := config.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("service: save chat %s: %w", space, err)
	}
	return nil
}

// lead is the per-space chat session. sem serialises conversations; mu
// guards proc and working; hmu guards chat.json.
type lead struct {
	sem chan struct{}
	hmu sync.Mutex

	mu      sync.Mutex
	proc    LeadProcess
	working bool
}

func (s *Service) leadFor(space string) *lead {
	s.leadMu.Lock()
	defer s.leadMu.Unlock()
	l := s.leads[space]
	if l == nil {
		l = &lead{sem: make(chan struct{}, 1)}
		s.leads[space] = l
	}
	return l
}

// LeadStatuses returns the state of every space's lead, keyed by space ID.
func (s *Service) LeadStatuses() map[string]string {
	spaces := s.ListSpaces()
	out := make(map[string]string, len(spaces))
	s.leadMu.Lock()
	defer s.leadMu.Unlock()
	for _, sp := range spaces {
		out[sp.ID] = LeadOffline
		l := s.leads[sp.ID]
		if l == nil {
			continue
		}
		l.mu.Lock()
		switch {
		case l.proc == nil:
		case l.working:
			out[sp.ID] = LeadWorking
		default:
			out[sp.ID] = LeadIdle
		}
		l.mu.Unlock()
	}
	return out
}

// Chat sends one message to the space's lead and returns its reply. The lead
// process is started on first use and kept for later messages; its Pi session
// is resumable, so a restart keeps the conversation. Messages to one space
// are handled one at a time in call order of arrival.
//
// Both the message and the reply (or the failure, as a system message) are
// appended to the space's persistent transcript, see ChatHistory.
func (s *Service) Chat(ctx context.Context, space, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("%w: empty message", ErrInvalidArgument)
	}
	sp, err := s.GetSpace(space)
	if err != nil {
		return "", err
	}
	l := s.leadFor(space)
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-l.sem }()

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return "", ErrShutdown
	}

	if err := s.recordChat(space, ChatMessage{Role: RoleUser, Text: text, At: time.Now().UTC()}); err != nil {
		return "", err
	}
	reply, err := s.converse(ctx, sp, l, text)
	rec := ChatMessage{Role: RoleLead, Text: reply, At: time.Now().UTC()}
	switch {
	case err != nil:
		rec.Role, rec.Text = RoleSystem, "error: "+err.Error()
	case strings.TrimSpace(reply) == "":
		return reply, nil // nothing to record
	}
	if rerr := s.recordChat(space, rec); rerr != nil && err == nil {
		return reply, rerr
	}
	return reply, err
}

// converse runs one prompt/reply turn against the space's lead process,
// starting it when needed. The caller holds l.sem.
func (s *Service) converse(ctx context.Context, sp config.Space, l *lead, text string) (string, error) {
	space := sp.ID
	var err error

	l.mu.Lock()
	l.working = true
	proc := l.proc
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.working = false
		l.mu.Unlock()
	}()

	drop := func() {
		l.mu.Lock()
		if l.proc == proc && proc != nil {
			l.proc = nil
		}
		l.mu.Unlock()
		if proc != nil {
			_ = proc.Abort()
			_ = proc.Stop(5 * time.Second)
		}
	}

	if proc != nil {
		select {
		case <-proc.Done(): // died since the last message
			proc = nil
			l.mu.Lock()
			l.proc = nil
			l.mu.Unlock()
		default:
		}
	}
	if proc == nil {
		if err := s.checkBudget(space, ""); err != nil {
			return "", err
		}
		o := pi.LeadOptions{Common: s.common(agent.RoleLead), Space: space, Summary: spaceSummary(sp.Projects)}
		if len(sp.Projects) > 0 {
			o.Dir = sp.Projects[0].Path
		}
		if fi, err := os.Stat(o.Dir); o.Dir != "" && (err != nil || !fi.IsDir()) {
			o.Dir = "" // checkout moved away: run without a working directory
		}
		start := s.opts.StartLead
		if start == nil {
			start = defaultLeadStarter
		}
		proc, err = start(s.ctx, o)
		if err != nil {
			return "", fmt.Errorf("service: start lead %s: %w", space, err)
		}
		l.mu.Lock()
		l.proc = proc
		l.mu.Unlock()
		// The intro turn is not part of the conversation: wait it out.
		intro, cancel := context.WithTimeout(ctx, leadStartTimeout)
		_, err := awaitSettled(intro, proc)
		cancel()
		if err != nil {
			drop()
			return "", fmt.Errorf("service: lead %s intro: %w", space, err)
		}
	}

	if err := proc.Prompt(text); err != nil {
		drop()
		return "", fmt.Errorf("service: lead %s: %w", space, err)
	}
	reply, err := awaitSettled(ctx, proc)
	if err != nil {
		drop()
		return "", err
	}
	return reply, nil
}

// spaceSummary describes the projects of a space for the lead's intro prompt.
func spaceSummary(ps []config.Project) string {
	var parts []string
	for _, p := range ps {
		line := p.ID
		if p.Summary != "" {
			line += ": " + strings.TrimSpace(p.Summary)
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "; ")
}

// stopLeads terminates the lead process of one space (all when space is empty).
func (s *Service) stopLeads(space string) {
	s.leadMu.Lock()
	var procs []LeadProcess
	for id, l := range s.leads {
		if space != "" && id != space {
			continue
		}
		l.mu.Lock()
		if l.proc != nil {
			procs = append(procs, l.proc)
			l.proc = nil
		}
		l.mu.Unlock()
	}
	s.leadMu.Unlock()
	for _, p := range procs {
		_ = p.Stop(5 * time.Second)
	}
}

// awaitSettled consumes proc's events until Pi reports it is settled and
// returns the text of the last assistant message of that run.
func awaitSettled(ctx context.Context, proc LeadProcess) (string, error) {
	var last, failure string
	events, done := proc.Events(), proc.Done()
	var grace <-chan time.Time // armed once the process has exited
	exited := errors.New("lead process exited")
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			// The reader closes events before done; give queued records a moment.
			done = nil
			if events == nil {
				return "", exited
			}
			grace = time.After(100 * time.Millisecond)
		case <-grace:
			return "", exited
		case ev, ok := <-events:
			if !ok {
				events = nil
				if done == nil {
					return "", exited
				}
				continue
			}
			switch ev.Type {
			case "message_end":
				if text, errText, ok := assistantMessage(ev.Raw); ok {
					if text != "" {
						last = text
					}
					if errText != "" {
						failure = errText
					}
				}
			case "response":
				var r struct {
					Success bool   `json:"success"`
					Error   string `json:"error"`
				}
				if json.Unmarshal(ev.Raw, &r) == nil && !r.Success && r.Error != "" {
					return "", errors.New("lead: " + r.Error)
				}
			case "agent_settled":
				if last == "" && failure != "" {
					return "", errors.New("lead: " + failure)
				}
				return last, nil
			}
		}
	}
}

// assistantMessage extracts the text (and error message) of an assistant
// message_end record. ok is false for other roles.
func assistantMessage(raw json.RawMessage) (text, errText string, ok bool) {
	var rec struct {
		Message struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &rec) != nil || rec.Message.Role != "assistant" {
		return "", "", false
	}
	var str string
	if json.Unmarshal(rec.Message.Content, &str) == nil {
		text = str
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(rec.Message.Content, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.Join(parts, "\n\n")
		}
	}
	if rec.Message.StopReason == "error" {
		errText = rec.Message.ErrorMessage
		if errText == "" {
			errText = "assistant run failed"
		}
	}
	return strings.TrimSpace(text), errText, true
}
