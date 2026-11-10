package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"fleet/pkg/schema"
)

func TestStatusColors(t *testing.T) {
	want := map[schema.Status]string{
		schema.StatusQueued:  "#F5C542",
		schema.StatusWorking: "#4C9AFF",
		schema.StatusReview:  "#B07CFF",
		schema.StatusDone:    "#3DD68C",
	}
	seen := map[string]bool{}
	for st, hex := range want {
		if StatusColor(st) != lipgloss.Color(hex) {
			t.Errorf("%s colour = %v, want %s", st, StatusColor(st), hex)
		}
		seen[hex] = true
	}
	if len(seen) != 4 {
		t.Error("status colours must be distinct")
	}
}

func TestStatusTagText(t *testing.T) {
	s := NewStyles()
	for _, st := range schema.Statuses() {
		got := ansi.Strip(s.StatusTag(st))
		if got != " "+strings.ToUpper(string(st))+" " {
			t.Errorf("tag(%s) = %q", st, got)
		}
	}
}

func TestFormatUSD(t *testing.T) {
	cases := map[float64]string{0: "$0.00", 0.4: "$0.40", 12.345: "$12.35", 150: "$150", 1250: "$1.2k", -3: "$0.00"}
	for in, want := range cases {
		if got := FormatUSD(in); got != want {
			t.Errorf("FormatUSD(%v) = %q, want %q", in, got, want)
		}
	}
	if got := SpendLabel(12.5, 50); got != "$12.50 / $50.00" {
		t.Errorf("SpendLabel = %q", got)
	}
	if got := SpendLabel(12.5, 0); got != "$12.50" {
		t.Errorf("unlimited SpendLabel = %q", got)
	}
}

func TestMeter(t *testing.T) {
	s := NewStyles()
	count := func(m string) (on, off int) {
		p := ansi.Strip(m)
		return strings.Count(p, "█"), strings.Count(p, "░")
	}
	cases := []struct {
		spent, limit float64
		on           int
	}{
		{0, 50, 0}, {25, 50, 5}, {50, 50, 10}, {500, 50, 10}, {0.01, 50, 1}, {10, 0, 0},
	}
	for _, c := range cases {
		on, off := count(s.Meter(c.spent, c.limit, 10))
		if on != c.on || on+off != 10 {
			t.Errorf("Meter(%v,%v) on=%d off=%d, want on=%d total=10", c.spent, c.limit, on, off, c.on)
		}
	}
	if s.Meter(1, 2, 0) != "" {
		t.Error("zero width meter must be empty")
	}
}

func TestPaneDimensions(t *testing.T) {
	s := NewStyles()
	body := strings.Repeat("a very long line that must be truncated to fit\n", 20)
	for _, focused := range []bool{true, false} {
		out := s.Pane("Title", s.Subtle.Render("aside"), body, 30, 10, focused)
		assertBox(t, out, 30, 10)
		if lipglossWidth(out) != 30 {
			t.Errorf("pane width = %d, want 30", lipglossWidth(out))
		}
	}
	if s.Pane("x", "", "y", 3, 3, false) != "" {
		t.Error("tiny pane should render nothing")
	}
}

func TestTabs(t *testing.T) {
	s := NewStyles()
	out := ansi.Strip(s.Tabs([]string{"A", "B"}, 1))
	if !strings.Contains(out, "A") || !strings.Contains(out, "B") {
		t.Errorf("tabs = %q", out)
	}
}

func TestSanitize(t *testing.T) {
	got := sanitize("a\x1b[31mred\x1b[0m\tb\r\nc\x00\x07")
	if got != "ared    b\nc" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestSpread(t *testing.T) {
	out := spread("left", "right", 20)
	if ansi.StringWidth(out) != 20 || !strings.HasPrefix(out, "left") || !strings.HasSuffix(out, "right") {
		t.Errorf("spread = %q", out)
	}
	if ansi.StringWidth(spread("a very long left side", "right", 12)) > 12 {
		t.Error("spread overflowed")
	}
}
