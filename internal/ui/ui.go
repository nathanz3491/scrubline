// Package ui is the timeline browser: snapshots on the left, the diff on the
// right, one key to put the tree back.
//
// Git calls are made synchronously inside Update rather than through commands.
// Diffing two trees in a local repository takes single-digit milliseconds, and
// keeping the model synchronous is what makes every keybinding testable without
// a terminal.
package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nathanz3491/scrubline/internal/meta"
	"github.com/nathanz3491/scrubline/internal/timeline"
)

// DiffMode selects what the right pane compares.
type DiffMode int

const (
	// DiffVsNow shows what changed between the selected snapshot and the
	// working tree as it is now -- what restoring would undo.
	DiffVsNow DiffMode = iota
	// DiffVsPrevious shows what the selected snapshot itself changed.
	DiffVsPrevious
)

func (m DiffMode) String() string {
	if m == DiffVsNow {
		return "vs now"
	}
	return "vs previous"
}

// Model is the timeline browser.
type Model struct {
	tl    *timeline.Timeline
	snaps []timeline.Snapshot

	// visible indexes snaps, and is what the filter narrows.
	visible []int
	cursor  int

	mode    DiffMode
	diff    []string
	diffTop int
	current string // tree id of the working tree as it is now

	filtering  bool
	filter     string
	confirming bool

	status string
	err    error

	width, height int
	quitting      bool

	// files caches the paths each snapshot touched, for filtering.
	files map[string][]string
}

// New builds a model over an already-open timeline.
func New(tl *timeline.Timeline) (*Model, error) {
	m := &Model{tl: tl, mode: DiffVsNow, width: 80, height: 24, files: map[string][]string{}}
	if err := m.reload(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Model) reload() error {
	snaps, err := m.tl.List(0)
	if err != nil {
		return err
	}
	m.snaps = snaps
	current, err := m.tl.CurrentTree()
	if err != nil {
		return err
	}
	m.current = current
	m.applyFilter()
	m.loadDiff()
	return nil
}

// Snapshots exposes the loaded snapshots, for tests.
func (m *Model) Snapshots() []timeline.Snapshot { return m.snaps }

// Selected is the snapshot under the cursor, if there is one.
func (m *Model) Selected() (timeline.Snapshot, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return timeline.Snapshot{}, false
	}
	return m.snaps[m.visible[m.cursor]], true
}

// Mode reports what the diff pane is comparing, for tests.
func (m *Model) Mode() DiffMode { return m.mode }

// Filter reports the active filename filter, for tests.
func (m *Model) Filter() string { return m.filter }

// Confirming reports whether a restore confirmation is pending, for tests.
func (m *Model) Confirming() bool { return m.confirming }

// Status is the last message shown in the footer, for tests.
func (m *Model) Status() string { return m.status }

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Some pseudo-terminals report 0x0 before the first real resize; keeping
		// the previous size beats rendering the "terminal too small" notice at
		// what is actually a perfectly usable window.
		if msg.Width > 0 && msg.Height > 0 {
			m.width, m.height = msg.Width, msg.Height
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending restore confirmation swallows every other key, so that the one
	// destructive action cannot be triggered by a stray keypress.
	if m.confirming {
		switch msg.String() {
		case "y", "Y":
			m.confirming = false
			m.restoreSelected()
		default:
			m.confirming = false
			m.status = "restore cancelled"
		}
		return m, nil
	}

	if m.filtering {
		switch msg.Type {
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyEsc:
			m.filtering = false
			m.filter = ""
			m.applyFilter()
			m.loadDiff()
		case tea.KeyBackspace:
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
				m.applyFilter()
				m.loadDiff()
			}
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
			if msg.Type == tea.KeySpace {
				m.filter += " "
			}
			m.applyFilter()
			m.loadDiff()
		}
		return m, nil
	}

	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit
	case "up":
		m.move(-1)
	case "down":
		m.move(1)
	case "pgup":
		m.diffTop = max(0, m.diffTop-m.diffHeight())
	case "pgdown":
		m.diffTop = min(max(0, len(m.diff)-1), m.diffTop+m.diffHeight())
	case "d":
		if m.mode == DiffVsNow {
			m.mode = DiffVsPrevious
		} else {
			m.mode = DiffVsNow
		}
		m.loadDiff()
	case "/":
		m.filtering = true
		m.status = ""
	case "r":
		if _, ok := m.Selected(); ok {
			m.confirming = true
		}
	}
	return m, nil
}

func (m *Model) move(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, len(m.visible)-1)
	m.diffTop = 0
	m.loadDiff()
}

func (m *Model) restoreSelected() {
	snap, ok := m.Selected()
	if !ok {
		return
	}
	res, err := m.tl.Restore(snap, timeline.RestoreOptions{})
	if err != nil {
		m.err = err
		m.status = fmt.Sprintf("restore failed: %v", err)
		return
	}
	changed := len(res.Plan)
	if err := m.reload(); err != nil {
		m.err = err
		return
	}
	m.status = fmt.Sprintf("restored %s, %d file%s changed -- undo with %s",
		snap.ShortID(), changed, plural(changed), res.Safety.ShortID())
}

// applyFilter narrows the visible list to snapshots that touched a matching
// path, keeping the cursor inside the result.
func (m *Model) applyFilter() {
	m.visible = m.visible[:0]
	needle := strings.ToLower(strings.TrimSpace(m.filter))
	for i, s := range m.snaps {
		if needle == "" || m.snapMatches(s, needle) {
			m.visible = append(m.visible, i)
		}
	}
	m.cursor = clamp(m.cursor, 0, max(0, len(m.visible)-1))
}

func (m *Model) snapMatches(s timeline.Snapshot, needle string) bool {
	if strings.Contains(strings.ToLower(s.Label), needle) {
		return true
	}
	for _, f := range m.filesOf(s) {
		if strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}

func (m *Model) filesOf(s timeline.Snapshot) []string {
	if cached, ok := m.files[s.ID]; ok {
		return cached
	}
	files, err := m.tl.Files(s)
	if err != nil {
		files = nil
	}
	m.files[s.ID] = files
	return files
}

func (m *Model) loadDiff() {
	m.diff = nil
	m.diffTop = 0
	snap, ok := m.Selected()
	if !ok {
		return
	}
	var from, to string
	switch m.mode {
	case DiffVsNow:
		from, to = snap.Tree, m.current
	default:
		parent, err := m.tl.ParentTree(snap)
		if err != nil {
			m.err = err
			return
		}
		from, to = parent, snap.Tree
	}
	text, err := m.tl.DiffText(from, to)
	if err != nil {
		m.err = err
		m.diff = []string{fmt.Sprintf("could not diff: %v", err)}
		return
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		m.diff = []string{"", "  no differences"}
		return
	}
	m.diff = strings.Split(text, "\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Run opens the browser on the terminal.
func Run(tl *timeline.Timeline) error {
	m, err := New(tl)
	if err != nil {
		return err
	}
	if len(m.snaps) == 0 {
		return fmt.Errorf("no snapshots yet -- run `%s watch` and start working", meta.Name)
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}
