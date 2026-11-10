package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// spaceItemHeight is the rows one space occupies: name, meter, summary, gap.
const spaceItemHeight = 4

// SpacesModel is the sidebar listing every registered space with its spend
// meter, lead-agent status and active task count.
type SpacesModel struct {
	styles        Styles
	spaces        []SpaceInfo
	cursor        int
	offset        int
	width, height int
}

// NewSpaces returns an empty sidebar.
func NewSpaces(st Styles) *SpacesModel { return &SpacesModel{styles: st} }

// SetSize sets the content area (inside the pane frame).
func (m *SpacesModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.ensureVisible()
}

// SetSpaces replaces the list, keeping the selection on the same space ID.
func (m *SpacesModel) SetSpaces(spaces []SpaceInfo) {
	id := m.SelectedID()
	m.spaces = append(m.spaces[:0:0], spaces...)
	m.cursor = 0
	for i, s := range m.spaces {
		if s.ID == id {
			m.cursor = i
			break
		}
	}
	m.ensureVisible()
}

// Spaces returns a copy of the current list.
func (m *SpacesModel) Spaces() []SpaceInfo { return append([]SpaceInfo(nil), m.spaces...) }

// Selected returns the highlighted space.
func (m *SpacesModel) Selected() (SpaceInfo, bool) {
	if m.cursor < 0 || m.cursor >= len(m.spaces) {
		return SpaceInfo{}, false
	}
	return m.spaces[m.cursor], true
}

// SelectedID returns the highlighted space ID or "".
func (m *SpacesModel) SelectedID() string {
	s, _ := m.Selected()
	return s.ID
}

// Totals sums spend and budget over all spaces.
func (m *SpacesModel) Totals() (spent, budget float64) {
	for _, s := range m.spaces {
		spent += s.SpendUSD
		if s.BudgetUSD > 0 {
			budget += s.BudgetUSD
		}
	}
	return spent, budget
}

func (m *SpacesModel) visible() int { return max(1, (m.height+1)/spaceItemHeight) }

func (m *SpacesModel) ensureVisible() {
	m.cursor = clamp(m.cursor, 0, len(m.spaces)-1)
	v := m.visible()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+v {
		m.offset = m.cursor - v + 1
	}
	m.offset = clamp(m.offset, 0, max(0, len(m.spaces)-v))
}

// Update handles navigation keys.
func (m *SpacesModel) Update(msg tea.Msg) tea.Cmd {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch km.String() {
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.spaces) - 1
	default:
		return nil
	}
	m.ensureVisible()
	return nil
}

// View renders the visible slice of spaces.
func (m *SpacesModel) View() string {
	if len(m.spaces) == 0 {
		return m.styles.Subtle.Render(wrap("No spaces registered. Run `fleet add`.", max(1, m.width)))
	}
	var b strings.Builder
	end := min(len(m.spaces), m.offset+m.visible())
	for i := m.offset; i < end; i++ {
		b.WriteString(m.renderItem(m.spaces[i], i == m.cursor))
		if i < end-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

func (m *SpacesModel) renderItem(s SpaceInfo, selected bool) string {
	marker, name := "  ", m.styles.Text
	if selected {
		marker, name = m.styles.Accent.Render("▌ "), m.styles.Bold
	}
	w := max(1, m.width-2)
	line1 := spread(name.Render(truncate(s.Name, w-2)), m.styles.LeadDot(s.Lead), w)
	label := SpendLabel(s.SpendUSD, s.BudgetUSD)
	meterW := clamp(w-lipgloss.Width(label)-1, 3, 12)
	line2 := m.styles.Meter(s.SpendUSD, s.BudgetUSD, meterW) + " " + m.styles.Subtle.Render(label)
	line3 := m.styles.Subtle.Render(fmt.Sprintf("lead %s · %d active", s.Lead, s.ActiveTasks))
	pad := func(l string) string { return marker + truncate(l, w) }
	return strings.Join([]string{pad(line1), pad(line2), pad(line3)}, "\n")
}
