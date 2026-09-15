package ui

import (
	"fmt"
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRenderManual prints the view so it can be eyeballed. Run with:
//
//	SCRUBLINE_RENDER=1 go test ./internal/ui -run TestRenderManual -v
func TestRenderManual(t *testing.T) {
	if os.Getenv("SCRUBLINE_RENDER") == "" {
		t.Skip("set SCRUBLINE_RENDER=1 to print the view")
	}
	m, _ := newModel(t)
	for _, size := range []struct{ w, h int }{{100, 24}, {60, 15}} {
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		fmt.Printf("\n===== %dx%d =====\n%s", size.w, size.h, m.View())
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.press("r")
	fmt.Printf("\n===== confirm prompt =====\n%s", m.View())
}
