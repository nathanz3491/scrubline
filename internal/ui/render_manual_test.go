package ui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRenderManual prints the view so it can be eyeballed. Run with:
//
//	go test ./internal/ui -run TestRenderManual -v
func TestRenderManual(t *testing.T) {
	m, _ := newModel(t)
	for _, size := range []struct{ w, h int }{{100, 24}, {60, 15}} {
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		fmt.Printf("\n===== %dx%d =====\n%s", size.w, size.h, m.View())
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.press("r")
	fmt.Printf("\n===== confirm prompt =====\n%s", m.View())
}
