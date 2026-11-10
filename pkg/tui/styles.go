package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"fleet/pkg/pi"
	"fleet/pkg/schema"
)

// Palette. Status colours are fixed (they carry meaning); neutrals adapt to
// light and dark terminal backgrounds.
var (
	colorQueued  = lipgloss.Color("#F5C542") // yellow
	colorWorking = lipgloss.Color("#4C9AFF") // blue
	colorReview  = lipgloss.Color("#B07CFF") // purple
	colorDone    = lipgloss.Color("#3DD68C") // green
	colorClosed  = lipgloss.Color("#7A8194") // grey
	colorDanger  = lipgloss.Color("#FF5C6C") // red
	colorAccent  = lipgloss.AdaptiveColor{Light: "#3B5BDB", Dark: "#7AA2F7"}
	colorText    = lipgloss.AdaptiveColor{Light: "#1F2430", Dark: "#E5E7EB"}
	colorMuted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#7A8194"}
	colorBorder  = lipgloss.AdaptiveColor{Light: "#C4C9D4", Dark: "#3B4252"}
)

// StatusColor returns the colour that represents a task status.
func StatusColor(s schema.Status) lipgloss.TerminalColor {
	switch s {
	case schema.StatusQueued:
		return colorQueued
	case schema.StatusWorking:
		return colorWorking
	case schema.StatusReview:
		return colorReview
	case schema.StatusDone:
		return colorDone
	}
	return colorClosed
}

// Styles bundles every Lipgloss style used by the TUI.
type Styles struct {
	App         lipgloss.Style
	Title       lipgloss.Style
	Subtle      lipgloss.Style
	Text        lipgloss.Style
	Bold        lipgloss.Style
	Accent      lipgloss.Style
	Danger      lipgloss.Style
	Success     lipgloss.Style
	Warning     lipgloss.Style
	Tab         lipgloss.Style
	ActiveTab   lipgloss.Style
	TabGap      lipgloss.Style
	Status      lipgloss.Style
	StatusError lipgloss.Style
	Prompt      lipgloss.Style
	DiffAdd     lipgloss.Style
	DiffDel     lipgloss.Style
	DiffHunk    lipgloss.Style
	DiffMeta    lipgloss.Style
	ChatUser    lipgloss.Style
	ChatLead    lipgloss.Style
	ChatSystem  lipgloss.Style
}

// NewStyles returns the default theme.
func NewStyles() Styles {
	base := lipgloss.NewStyle()
	return Styles{
		App:         base.Foreground(colorText),
		Title:       base.Bold(true).Foreground(colorAccent),
		Subtle:      base.Foreground(colorMuted),
		Text:        base.Foreground(colorText),
		Bold:        base.Bold(true).Foreground(colorText),
		Accent:      base.Foreground(colorAccent),
		Danger:      base.Foreground(colorDanger),
		Success:     base.Foreground(colorDone),
		Warning:     base.Foreground(colorQueued),
		Tab:         base.Foreground(colorMuted).Padding(0, 1),
		ActiveTab:   base.Bold(true).Foreground(colorText).Padding(0, 1).Underline(true).UnderlineSpaces(false),
		TabGap:      base.Foreground(colorBorder),
		Status:      base.Foreground(colorMuted),
		StatusError: base.Foreground(colorDanger).Bold(true),
		Prompt:      base.Foreground(colorAccent).Bold(true),
		DiffAdd:     base.Foreground(colorDone),
		DiffDel:     base.Foreground(colorDanger),
		DiffHunk:    base.Foreground(colorWorking),
		DiffMeta:    base.Foreground(colorMuted).Bold(true),
		ChatUser:    base.Foreground(colorAccent).Bold(true),
		ChatLead:    base.Foreground(colorReview).Bold(true),
		ChatSystem:  base.Foreground(colorMuted).Italic(true),
	}
}

// StatusTag renders a compact coloured badge such as " WORKING ".
func (s Styles) StatusTag(st schema.Status) string {
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#0B0E14")).
		Background(StatusColor(st)).
		Padding(0, 1).
		Render(strings.ToUpper(string(st)))
}

// StatusDot renders a coloured bullet for a status.
func (s Styles) StatusDot(st schema.Status) string {
	return lipgloss.NewStyle().Foreground(StatusColor(st)).Render("●")
}

// LeadDot renders the lead-agent indicator.
func (s Styles) LeadDot(l LeadStatus) string {
	switch l {
	case LeadIdle:
		return lipgloss.NewStyle().Foreground(colorDone).Render("●")
	case LeadWorking:
		return lipgloss.NewStyle().Foreground(colorWorking).Render("◉")
	}
	return lipgloss.NewStyle().Foreground(colorClosed).Render("○")
}

// budgetColor picks the meter colour for spent/limit.
func budgetColor(spent, limit float64) lipgloss.TerminalColor {
	switch {
	case limit <= 0:
		return colorAccent
	case spent >= limit:
		return colorDanger
	case spent >= limit*pi.DefaultWarnRatio:
		return colorQueued
	}
	return colorDone
}

// FormatUSD formats a dollar amount compactly ($0.42, $12.30, $1.2k).
func FormatUSD(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		v = 0
	}
	if v >= 1000 {
		return fmt.Sprintf("$%.1fk", v/1000)
	}
	if v >= 100 {
		return fmt.Sprintf("$%.0f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// SpendLabel renders "$X / $Y" (or "$X" when the budget is unlimited).
func SpendLabel(spent, limit float64) string {
	if limit <= 0 {
		return FormatUSD(spent)
	}
	return FormatUSD(spent) + " / " + FormatUSD(limit)
}

// Meter renders a width-cell spend bar coloured by budget state. Unlimited
// budgets render an empty (dim) track.
func (s Styles) Meter(spent, limit float64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := 0
	if limit > 0 && spent > 0 {
		filled = int(math.Round(spent / limit * float64(width)))
		if filled == 0 {
			filled = 1
		}
		if filled > width {
			filled = width
		}
	}
	on := lipgloss.NewStyle().Foreground(budgetColor(spent, limit)).Render(strings.Repeat("█", filled))
	off := lipgloss.NewStyle().Foreground(colorBorder).Render(strings.Repeat("░", width-filled))
	return on + off
}

// Tabs renders a tab bar with the given active index.
func (s Styles) Tabs(labels []string, active int) string {
	parts := make([]string, 0, len(labels))
	for i, l := range labels {
		if i == active {
			parts = append(parts, s.ActiveTab.Render(l))
		} else {
			parts = append(parts, s.Tab.Render(l))
		}
	}
	return strings.Join(parts, s.TabGap.Render("│"))
}

// Pane draws a rounded frame of exactly w×h cells containing a title row
// (title left, aside right) and the body. Focused panes get an accent border.
func (s Styles) Pane(title, aside, body string, w, h int, focused bool) string {
	if w < 5 || h < 3 {
		return ""
	}
	iw, ih := w-4, h-2 // border (2) + horizontal padding (2); border (2)
	ts := s.Subtle.Bold(true)
	bc := colorBorder
	if focused {
		ts = s.Title
		bc = colorAccent
	}
	head := ts.Render(truncate(title, iw))
	if aside != "" {
		gap := iw - lipgloss.Width(head) - lipgloss.Width(aside)
		if gap >= 1 {
			head += strings.Repeat(" ", gap) + aside
		}
	}
	content := head
	if ih > 1 {
		content += "\n" + fit(body, iw, ih-1)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(bc).
		Padding(0, 1).
		Width(w - 2).
		Height(h - 2).
		MaxWidth(w).
		MaxHeight(h).
		Render(content)
}
