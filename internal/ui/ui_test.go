package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nathanz3491/scrubline/internal/timeline"
)

// newModel builds a repository with three snapshots touching different files.
func newModel(t *testing.T) (*Model, string) {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")

	tl, err := timeline.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write("alpha.go", "package main\n\nfunc main() {}\n")
	if _, _, err := tl.Snap("first"); err != nil {
		t.Fatal(err)
	}
	write("beta.go", "package main\n\nvar x = 1\n")
	if _, _, err := tl.Snap(""); err != nil {
		t.Fatal(err)
	}
	write("alpha.go", "package main\n\nfunc main() { println(1) }\n")
	if _, _, err := tl.Snap("third"); err != nil {
		t.Fatal(err)
	}
	// Leave the working tree different from the newest snapshot so that a
	// "vs now" diff is non-empty.
	write("beta.go", "package main\n\nvar x = 2\n")

	m, err := New(tl)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, dir
}

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func (m *Model) press(keys ...string) {
	for _, k := range keys {
		m.Update(key(k))
	}
}

func TestArrowKeysMoveTheSelectionAndRepaintTheDiff(t *testing.T) {
	m, _ := newModel(t)

	first, ok := m.Selected()
	if !ok {
		t.Fatal("nothing selected")
	}
	diffBefore := strings.Join(m.diff, "\n")

	m.press("down")
	second, _ := m.Selected()
	if second.ID == first.ID {
		t.Fatal("down did not move the selection")
	}
	if strings.Join(m.diff, "\n") == diffBefore {
		t.Fatal("the diff pane did not change with the selection")
	}

	m.press("up")
	if back, _ := m.Selected(); back.ID != first.ID {
		t.Fatal("up did not move back")
	}

	// The cursor must not run off either end.
	m.press("up", "up", "up", "up")
	if m.cursor != 0 {
		t.Fatalf("cursor went above the top: %d", m.cursor)
	}
	m.press("down", "down", "down", "down", "down")
	if m.cursor != len(m.visible)-1 {
		t.Fatalf("cursor ran past the end: %d of %d", m.cursor, len(m.visible))
	}
}

func TestDTogglesDiffMode(t *testing.T) {
	m, _ := newModel(t)
	if m.Mode() != DiffVsNow {
		t.Fatalf("mode starts at %v, want vs now", m.Mode())
	}
	vsNow := strings.Join(m.diff, "\n")

	m.press("d")
	if m.Mode() != DiffVsPrevious {
		t.Fatal("d did not switch to vs previous")
	}
	if strings.Join(m.diff, "\n") == vsNow {
		t.Fatal("the diff did not change when the mode did")
	}

	m.press("d")
	if m.Mode() != DiffVsNow {
		t.Fatal("d did not toggle back")
	}
}

func TestSlashFiltersByFilename(t *testing.T) {
	m, _ := newModel(t)
	all := len(m.visible)
	if all != 3 {
		t.Fatalf("expected 3 snapshots, got %d", all)
	}

	m.press("/")
	if !m.filtering {
		t.Fatal("/ did not start filtering")
	}
	m.press("b", "e", "t", "a")
	if m.Filter() != "beta" {
		t.Fatalf("filter = %q", m.Filter())
	}
	if len(m.visible) == 0 || len(m.visible) >= all {
		t.Fatalf("filter matched %d of %d snapshots, expected a strict subset", len(m.visible), all)
	}
	for _, idx := range m.visible {
		s := m.snaps[idx]
		var touched bool
		for _, f := range m.filesOf(s) {
			if strings.Contains(f, "beta") {
				touched = true
			}
		}
		if !touched && !strings.Contains(s.Label, "beta") {
			t.Fatalf("snapshot %s matched the filter without touching beta", s.ShortID())
		}
	}

	m.press("enter")
	if m.filtering {
		t.Fatal("enter did not accept the filter")
	}

	m.press("/", "esc")
	if m.Filter() != "" || len(m.visible) != all {
		t.Fatalf("esc did not clear the filter: %q, %d visible", m.Filter(), len(m.visible))
	}
}

func TestFilterBackspace(t *testing.T) {
	m, _ := newModel(t)
	m.press("/", "b", "e", "t", "a", "backspace")
	if m.Filter() != "bet" {
		t.Fatalf("filter = %q, want %q", m.Filter(), "bet")
	}
}

func TestRestoreAsksForConfirmationFirst(t *testing.T) {
	m, dir := newModel(t)
	betaPath := filepath.Join(dir, "beta.go")
	before, err := os.ReadFile(betaPath)
	if err != nil {
		t.Fatal(err)
	}

	// r alone must not touch anything.
	m.press("r")
	if !m.Confirming() {
		t.Fatal("r did not ask for confirmation")
	}
	after, _ := os.ReadFile(betaPath)
	if string(after) != string(before) {
		t.Fatal("r modified the working tree before confirmation")
	}

	// Any other key cancels.
	m.press("n")
	if m.Confirming() {
		t.Fatal("n left the confirmation pending")
	}
	after, _ = os.ReadFile(betaPath)
	if string(after) != string(before) {
		t.Fatal("cancelling still modified the working tree")
	}
	if !strings.Contains(m.Status(), "cancelled") {
		t.Fatalf("status after cancel = %q", m.Status())
	}

	// y goes through.
	m.press("r", "y")
	if m.Confirming() {
		t.Fatal("confirmation still pending after y")
	}
	after, _ = os.ReadFile(betaPath)
	if string(after) == string(before) {
		t.Fatal("y did not restore")
	}
	if !strings.Contains(m.Status(), "restored") || !strings.Contains(m.Status(), "undo with") {
		t.Fatalf("status after restore = %q, want the safety snapshot id", m.Status())
	}
}

func TestConfirmationSwallowsOtherKeys(t *testing.T) {
	m, _ := newModel(t)
	mode := m.Mode()
	m.press("r")
	// d must not toggle the diff while a restore confirmation is pending.
	m.press("d")
	if m.Mode() != mode {
		t.Fatal("a pending confirmation let another keybinding through")
	}
	if m.Confirming() {
		t.Fatal("d should have cancelled the confirmation")
	}
}

func TestQuitSetsQuitting(t *testing.T) {
	m, _ := newModel(t)
	if _, cmd := m.Update(key("q")); cmd == nil {
		t.Fatal("q returned no quit command")
	}
	if !m.quitting {
		t.Fatal("q did not mark the model as quitting")
	}
}

func TestFooterShowsEveryKeybinding(t *testing.T) {
	m, _ := newModel(t)
	t.Setenv("NO_COLOR", "1")
	view := m.View()
	for _, hint := range []string{"↑/↓", "r restore", "d ", "/ filter", "q quit"} {
		if !strings.Contains(view, hint) {
			t.Fatalf("footer does not mention %q:\n%s", hint, view)
		}
	}
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func TestNoColorOutputHasNoEscapeCodes(t *testing.T) {
	m, _ := newModel(t)
	t.Setenv("NO_COLOR", "1")
	view := m.View()
	if ansi.MatchString(view) {
		t.Fatalf("NO_COLOR output contains escape codes:\n%q", view)
	}
}

func TestLayoutNeverExceedsTerminalWidth(t *testing.T) {
	m, _ := newModel(t)
	t.Setenv("NO_COLOR", "1")

	for _, size := range []struct{ w, h int }{
		{100, 30}, {80, 24}, {60, 15}, {40, 10}, {100, 30},
	} {
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		view := m.View()
		for i, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
			if w := lipgloss.Width(line); w > size.w {
				t.Fatalf("at %dx%d line %d is %d wide:\n%q", size.w, size.h, i, w, line)
			}
		}
		if lines := strings.Count(strings.TrimRight(view, "\n"), "\n") + 1; lines > size.h {
			t.Fatalf("at %dx%d the view is %d lines tall", size.w, size.h, lines)
		}
	}
}

func TestNarrowTerminalDropsTheDiffPane(t *testing.T) {
	m, _ := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	if _, showDiff := m.layout(); showDiff {
		t.Fatal("the diff pane was kept at a width too small for two columns")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if _, showDiff := m.layout(); !showDiff {
		t.Fatal("the diff pane did not come back when there was room")
	}
}

func TestViewSurvivesAnEmptyFilterResult(t *testing.T) {
	m, _ := newModel(t)
	t.Setenv("NO_COLOR", "1")
	m.press("/", "z", "z", "z", "z")
	if len(m.visible) != 0 {
		t.Fatalf("expected no matches, got %d", len(m.visible))
	}
	view := m.View() // must not panic
	if !strings.Contains(view, "no snapshots match") {
		t.Fatalf("view does not say the filter matched nothing:\n%s", view)
	}
	// r with nothing selected must not ask to restore anything.
	m.press("r")
	if m.Confirming() {
		t.Fatal("r asked to restore with no selection")
	}
}

func TestZeroSizedWindowMessageIsIgnored(t *testing.T) {
	m, _ := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(tea.WindowSizeMsg{Width: 0, Height: 0})
	if m.width != 100 || m.height != 30 {
		t.Fatalf("a 0x0 resize clobbered the size: %dx%d", m.width, m.height)
	}
}
