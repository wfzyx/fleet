package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// usageJSON renders a Pi usage object with synthetic numbers.
func usageJSON(in, out, cr, cw int, total float64) string {
	return fmt.Sprintf(`{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":%d,"totalTokens":%d,"cost":{"input":%g,"output":%g,"cacheRead":0,"cacheWrite":0,"total":%g}}`,
		in, out, cr, cw, in+out+cr+cw, total/2, total/2, total)
}

func assistantLine(id, ts, provider, model, usage string) string {
	return fmt.Sprintf(`{"type":"message","id":"%s","parentId":null,"timestamp":"%s","message":{"role":"assistant","provider":"%s","model":"%s","usage":%s,"stopReason":"stop"}}`,
		id, ts, provider, model, usage)
}

// sampleSession returns a session with two models, a usage entry, a
// compaction, a duplicate id, a user message and a torn final line.
func sampleSession() string {
	lines := []string{
		`{"type":"session","version":3,"id":"sess-1","timestamp":"2026-01-01T10:00:00.000Z","cwd":"/work/repo"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-01-01T10:00:01.000Z","provider":"anthropic","modelId":"claude-sonnet-4-5"}`,
		`{"type":"session_info","id":"n1","parentId":null,"timestamp":"2026-01-01T10:00:01.500Z","name":"my task"}`,
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T10:00:02.000Z","message":{"role":"user","content":"hi"}}`,
		assistantLine("a1", "2026-01-01T10:01:00.000Z", "anthropic", "claude-sonnet-4-5", usageJSON(100, 50, 1000, 10, 0.10)),
		assistantLine("a2", "2026-01-01T10:02:00.000Z", "anthropic", "claude-opus-5-5", usageJSON(200, 100, 0, 0, 0.50)),
		// duplicate entry id must not double count
		assistantLine("a2", "2026-01-01T10:02:00.000Z", "anthropic", "claude-opus-5-5", usageJSON(200, 100, 0, 0, 0.50)),
		`{"type":"usage","id":"us1","parentId":null,"timestamp":"2026-01-01T10:03:00.000Z","kind":"cache_warm","provider":"anthropic","model":"claude-sonnet-4-5","usage":` + usageJSON(0, 0, 5000, 0, 0.015) + `}`,
		// compaction has no model: attributed to the current model (sonnet)
		`{"type":"compaction","id":"c1","parentId":null,"timestamp":"2026-01-01T10:04:00.000Z","summary":"s","firstKeptEntryId":"c1","tokensBefore":10,"usage":` + usageJSON(10, 5, 0, 0, 0.01) + `}`,
		// error message with zero usage: ignored for cost
		assistantLine("a3", "2026-01-01T10:05:00.000Z", "anthropic", "claude-opus-5-5", usageJSON(0, 0, 0, 0, 0)),
		// long idle gap, then more activity
		assistantLine("a4", "2026-01-01T14:05:00.000Z", "anthropic", "claude-sonnet-4-5", usageJSON(10, 10, 0, 0, 0.02)),
		`{"type":"message","id":"u2","parentId":null,"timestamp":"2026-01-01T14:06:00.000Z","message":{"role":"user","content":"more"}}`,
		`{"type":"message","id":"a5","parentId":null,"timesta`, // torn write
	}
	return strings.Join(lines, "\n")
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
