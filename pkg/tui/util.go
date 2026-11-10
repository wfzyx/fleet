package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// truncate shortens s to at most w display cells, appending an ellipsis.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// fit forces s into exactly h lines, each at most w cells wide.
func fit(s string, w, h int) string {
	if h <= 0 || w <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = truncate(l, w)
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// spread places left and right on one w-wide line, truncating left if needed.
func spread(left, right string, w int) string {
	rw := ansi.StringWidth(right)
	if rw >= w {
		return truncate(right, w)
	}
	left = truncate(left, w-rw-1)
	gap := w - ansi.StringWidth(left) - rw
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// sanitize removes ANSI sequences and control characters from untrusted text
// (agent logs, diffs) so it cannot corrupt the layout; tabs become 4 spaces.
func sanitize(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f) {
			return r
		}
		return -1
	}, s)
}

// wrap hard-wraps s to w cells (word boundaries preferred).
func wrap(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Wrap(s, w, "")
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	return max(lo, min(v, hi))
}
