package pi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAssistantMessage(t *testing.T) {
	cases := []struct {
		name, raw, text, errText string
		ok                       bool
	}{
		{"text blocks", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"text","text":"there"}]}}`, "hi\n\nthere", "", true},
		{"string content", `{"type":"message_end","message":{"role":"assistant","content":"hello"}}`, "hello", "", true},
		{"user ignored", `{"type":"message_end","message":{"role":"user","content":"skip me"}}`, "", "", false},
		{"error stop reason", `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"quota exceeded"}}`, "", "quota exceeded", true},
		{"error without message", `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error"}}`, "", "error stop reason", true},
		{"not json", `not json`, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, errText, ok := assistantMessage(json.RawMessage(tc.raw))
			if ok != tc.ok || text != tc.text || errText != tc.errText {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", text, errText, ok, tc.text, tc.errText, tc.ok)
			}
		})
	}
}

func TestAwaitSettledErrors(t *testing.T) {
	proc := &Process{
		events: make(chan Event, 10),
		done:   make(chan struct{}),
	}
	proc.events <- Event{Type: "message_end", Raw: json.RawMessage(`{"message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"quota"}}`)}
	proc.events <- Event{Type: "agent_settled", Raw: json.RawMessage(`{"type":"agent_settled"}`)}
	if _, err := AwaitSettled(context.Background(), proc); err == nil || !strings.Contains(err.Error(), "quota") {
		t.Errorf("error stop reason ignored: %v", err)
	}

	proc = &Process{
		events: make(chan Event, 10),
		done:   make(chan struct{}),
	}
	proc.events <- Event{Type: "response", Raw: json.RawMessage(`{"type":"response","command":"prompt","success":false,"error":"bad"}`)}
	if _, err := AwaitSettled(context.Background(), proc); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("response error ignored: %v", err)
	}

	proc = &Process{
		events: make(chan Event, 10),
		done:   make(chan struct{}),
	}
	close(proc.events)
	close(proc.done)
	if _, err := AwaitSettled(context.Background(), proc); err == nil {
		t.Error("closed event stream without settle returned no error")
	}
}
