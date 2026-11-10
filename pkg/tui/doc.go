// Package tui is the native Bubble Tea cockpit for the fleet: a spaces
// sidebar, task lanes, a live lead-agent chat and a task inspector.
//
// The package is a pure presentation layer. All data flows in and out through
// the Backend interface and plain tea.Msg values, so every component can be
// unit tested without a terminal or a running agent.
package tui
