package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/nathanz3491/scrubline/internal/meta"
	"github.com/nathanz3491/scrubline/internal/timeline"
)

// Colors are adaptive so the same palette is legible on light and dark
// terminals. When NO_COLOR is set every style collapses to plain text.
//
// These are built on first use rather than at package level on purpose:
// constructing a lipgloss style creates the default renderer, which asks the
// terminal for its background colour. Doing that at init made every command --
// `list`, `snap`, `restore` -- emit a terminal query and wait for an answer,
// which costs startup time and hangs outright on a terminal that never replies.
// Only the browser needs colour, so only the browser pays for it.
var (
	stylesOnce   sync.Once
	styleAdded   lipgloss.Style
	styleRemoved lipgloss.Style
	styleHunk    lipgloss.Style
	styleFile    lipgloss.Style
	styleDim     lipgloss.Style
	styleCursor  lipgloss.Style
	styleLabel   lipgloss.Style
	styleWarn    lipgloss.Style
)

func initStyles() {
	stylesOnce.Do(func() {
		styleAdded = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#116329", Dark: "#3fb950"})
		styleRemoved = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#a40e26", Dark: "#f85149"})
		styleHunk = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#0550ae", Dark: "#79c0ff"})
		styleFile = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#24292f", Dark: "#e6edf3"})
		styleDim = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#6e7781", Dark: "#8b949e"})
		styleCursor = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#0550ae", Dark: "#79c0ff"})
		styleLabel = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8250df", Dark: "#d2a8ff"})
		styleWarn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#a40e26", Dark: "#f85149"})
	})
}

// plainOutput reports whether color must be suppressed entirely.
func plainOutput() bool { return os.Getenv("NO_COLOR") != "" }

// paint applies one of the lazily built styles. The style is named by a getter
// rather than passed in, so that NO_COLOR output never constructs one at all.
func paint(get func() lipgloss.Style, s string) string {
	if plainOutput() {
		return s
	}
	initStyles()
	return get().Render(s)
}

func sAdded() lipgloss.Style   { return styleAdded }
func sRemoved() lipgloss.Style { return styleRemoved }
func sHunk() lipgloss.Style    { return styleHunk }
func sFile() lipgloss.Style    { return styleFile }
func sDim() lipgloss.Style     { return styleDim }
func sCursor() lipgloss.Style  { return styleCursor }
func sLabel() lipgloss.Style   { return styleLabel }
func sWarn() lipgloss.Style    { return styleWarn }

// layout splits the terminal into a snapshot pane and a diff pane. Below a
// certain width there is no honest way to show both, so the diff pane is
// dropped rather than rendered into a corrupted two-column layout.
func (m *Model) layout() (listWidth int, showDiff bool) {
	if m.width < 56 {
		return m.width, false
	}
	// The list needs about 30 columns before the file count starts truncating;
	// below that, give the diff pane whatever is left rather than squeezing both.
	listWidth = clamp(m.width/3, 30, 44)
	if m.width-listWidth-3 < 22 {
		listWidth = m.width - 25
	}
	return listWidth, true
}

func (m *Model) diffHeight() int {
	h := m.height - 4 // header, separator, footer, status
	if h < 1 {
		return 1
	}
	return h
}

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	if m.height < 6 || m.width < 20 {
		return fmt.Sprintf("%s needs a slightly larger terminal\n", meta.Name)
	}

	listWidth, showDiff := m.layout()
	bodyHeight := m.diffHeight()

	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")

	listLines := m.listLines(listWidth, bodyHeight)
	if !showDiff {
		for _, ln := range listLines {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	} else {
		diffLines := m.diffPane(m.width-listWidth-3, bodyHeight)
		for i := 0; i < bodyHeight; i++ {
			left := ""
			if i < len(listLines) {
				left = listLines[i]
			}
			right := ""
			if i < len(diffLines) {
				right = diffLines[i]
			}
			b.WriteString(padTo(left, listWidth))
			b.WriteString(paint(sDim, " │ "))
			b.WriteString(right)
			b.WriteString("\n")
		}
	}

	b.WriteString(m.footer())
	return b.String()
}

func (m *Model) header() string {
	title := fmt.Sprintf("%s  %d snapshot%s", meta.Name, len(m.snaps), plural(len(m.snaps)))
	right := m.mode.String()
	if m.filter != "" {
		right = fmt.Sprintf("filter %q  %s", m.filter, right)
	}
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(title, m.width)
	}
	return paint(sFile, title) + strings.Repeat(" ", gap) + paint(sDim, right)
}

func (m *Model) listLines(width, height int) []string {
	if len(m.visible) == 0 {
		return []string{paint(sDim, truncate("  no snapshots match", width))}
	}

	// Scroll the list so the cursor stays on screen.
	top := 0
	if m.cursor >= height {
		top = m.cursor - height + 1
	}

	var lines []string
	for i := top; i < len(m.visible) && len(lines) < height; i++ {
		s := m.snaps[m.visible[i]]
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}
		row := fmt.Sprintf("%s%s  %s", marker, s.ShortID(), relative(s))
		row = truncate(row, width)
		if i == m.cursor {
			row = paint(sCursor, row)
		}
		lines = append(lines, row)

		// The label gets its own line so that a long one never pushes the id
		// off the row.
		if s.Label != "" && len(lines) < height {
			label := truncate("     "+s.Label, width)
			lines = append(lines, paint(sLabel, label))
		}
	}
	return lines
}

func (m *Model) diffPane(width, height int) []string {
	if width < 4 {
		return nil
	}
	var lines []string
	for i := m.diffTop; i < len(m.diff) && len(lines) < height; i++ {
		lines = append(lines, colorizeDiffLine(m.diff[i], width))
	}
	return lines
}

func colorizeDiffLine(line string, width int) string {
	text := truncate(line, width)
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return paint(sDim, text)
	case strings.HasPrefix(line, "diff "), strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"),
		strings.HasPrefix(line, "similarity"), strings.HasPrefix(line, "rename "):
		return paint(sFile, text)
	case strings.HasPrefix(line, "@@"):
		return paint(sHunk, text)
	case strings.HasPrefix(line, "+"):
		return paint(sAdded, text)
	case strings.HasPrefix(line, "-"):
		return paint(sRemoved, text)
	default:
		return text
	}
}

func (m *Model) footer() string {
	var status string
	switch {
	case m.confirming:
		snap, _ := m.Selected()
		status = paint(sWarn, fmt.Sprintf("restore %s over the working tree?  y / n", snap.ShortID()))
	case m.filtering:
		status = fmt.Sprintf("filter: %s_", m.filter)
	case m.status != "":
		status = paint(sDim, m.status)
	default:
		snap, ok := m.Selected()
		if ok {
			status = paint(sDim, fmt.Sprintf("%s  %s  %d file%s changed",
				snap.ShortID(), snap.Branch, snap.Files, plural(snap.Files)))
		}
	}

	help := "↑/↓ move   r restore   d " + m.otherMode() + "   / filter   q quit"
	if m.width < 72 {
		help = "↑/↓ move  r restore  d diff  / filter  q quit"
	}
	if m.filtering {
		help = "type to filter by filename   enter accept   esc clear"
	}
	if m.confirming {
		help = "y restore   any other key cancel"
	}
	return truncate(status, m.width) + "\n" + paint(sDim, truncate(help, m.width))
}

func (m *Model) otherMode() string {
	if m.mode == DiffVsNow {
		return "diff vs previous"
	}
	return "diff vs now"
}

// relative renders the age and size of a snapshot for the list row.
func relative(s timeline.Snapshot) string {
	return fmt.Sprintf("%-8s %d file%s", relativeTime(time.Since(s.When)), s.Files, plural(s.Files))
}

func relativeTime(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// padTo pads a rendered cell out to width, accounting for escape sequences.
func padTo(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// truncate cuts a plain string to width runes, leaving room for an ellipsis.
// It is applied before styling so escape sequences are never cut in half.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}
