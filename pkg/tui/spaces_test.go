package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func newSpaces(w, h int) *SpacesModel {
	m := NewSpaces(NewStyles())
	m.SetSize(w, h)
	m.SetSpaces(newFake().spaces)
	return m
}

func TestSpacesNavigation(t *testing.T) {
	m := newSpaces(26, 20)
	if m.SelectedID() != "s1" {
		t.Fatalf("initial = %q", m.SelectedID())
	}
	m.Update(keyMsg("down"))
	if m.SelectedID() != "s2" {
		t.Errorf("down = %q", m.SelectedID())
	}
	m.Update(keyMsg("down")) // clamped
	if m.SelectedID() != "s2" {
		t.Errorf("clamp = %q", m.SelectedID())
	}
	m.Update(keyMsg("k"))
	m.Update(keyMsg("up"))
	if m.SelectedID() != "s1" {
		t.Errorf("up = %q", m.SelectedID())
	}
	m.Update(keyMsg("G"))
	if m.SelectedID() != "s2" {
		t.Errorf("G = %q", m.SelectedID())
	}
	m.Update(keyMsg("g"))
	if m.SelectedID() != "s1" {
		t.Errorf("g = %q", m.SelectedID())
	}
}

func TestSpacesKeepsSelectionAcrossRefresh(t *testing.T) {
	m := newSpaces(26, 20)
	m.Update(keyMsg("down"))
	sp := newFake().spaces
	sp[1].SpendUSD = 49
	m.SetSpaces([]SpaceInfo{{ID: "s0", Name: "new"}, sp[0], sp[1]})
	if m.SelectedID() != "s2" {
		t.Errorf("selection lost: %q", m.SelectedID())
	}
	m.SetSpaces(nil)
	if _, ok := m.Selected(); ok {
		t.Error("empty list should have no selection")
	}
	if !strings.Contains(ansi.Strip(m.View()), "No spaces") {
		t.Error("empty state missing")
	}
}

func TestSpacesViewShowsMeterLeadAndCount(t *testing.T) {
	m := newSpaces(28, 20)
	v := ansi.Strip(m.View())
	for _, want := range []string{"alpha", "beta", "$12.50 / $50.00", "$45.00 / $50.00", "3 active", "lead idle", "lead working", "█"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	spent, budget := m.Totals()
	if spent != 57.5 || budget != 100 {
		t.Errorf("totals = %v/%v", spent, budget)
	}
}

func TestSpacesScrolls(t *testing.T) {
	var sp []SpaceInfo
	for i := 0; i < 10; i++ {
		sp = append(sp, SpaceInfo{ID: string(rune('a' + i)), Name: "space-" + string(rune('a'+i))})
	}
	m := NewSpaces(NewStyles())
	m.SetSize(26, 8) // two items
	m.SetSpaces(sp)
	for i := 0; i < 9; i++ {
		m.Update(keyMsg("j"))
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "space-j") || strings.Contains(v, "space-a") {
		t.Errorf("window did not follow cursor:\n%s", v)
	}
	assertBox(t, m.View(), 26, 8-1) // 2 items * 3 lines + 1 separator... bounded by height
}
