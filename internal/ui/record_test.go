package ui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/nathanz3491/scrubline/internal/timeline"
)

// TestRecordDemo writes docs/demo.cast from the browser's real output.
//
// Driving the browser through a pseudo-terminal turned out to be the wrong
// tool: bubbletea reads stdin while it waits for the terminal to answer its
// startup queries, so an unattended pty swallows the keystrokes. This records
// the same thing from the other side -- a real repository, the real model, real
// key messages, and the frames View actually produced -- and writes them as an
// asciicast. Everything on screen is the tool's own output; only the frame
// timing is chosen here.
//
//	SCRUBLINE_RECORD=1 go test ./internal/ui -run TestRecordDemo
//	agg docs/demo.cast docs/demo.gif
func TestRecordDemo(t *testing.T) {
	if os.Getenv("SCRUBLINE_RECORD") == "" {
		t.Skip("set SCRUBLINE_RECORD=1 to write docs/demo.cast")
	}

	const (
		cols = 96
		rows = 24
	)

	// A test has no terminal, so lipgloss would otherwise resolve every style
	// to plain text. The recording wants the colours a real dark terminal gets.
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	repo := filepath.Join(dir, "parser")
	buildDemoRepo(t, repo)

	tl, err := timeline.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(tl)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: cols, Height: rows})

	var cast castWriter
	cast.header(cols, rows)

	// The timeline as the agent left it: three recorded turns, the newest of
	// which is the one that broke the build.
	cast.frame(m.View(), 2.2)

	// Scrub back one turn.
	m.Update(key("down"))
	cast.frame(m.View(), 2.0)

	// Look at what that turn changed, then back to the diff against now.
	m.Update(key("d"))
	cast.frame(m.View(), 2.0)
	m.Update(key("d"))
	cast.frame(m.View(), 1.3)

	// Put the tree back.
	m.Update(key("r"))
	cast.frame(m.View(), 1.4)
	m.Update(key("y"))
	cast.frame(m.View(), 2.2)

	// Quit, and show the deleted file really is back on disk.
	m.Update(key("q"))
	cast.shell(repo, "ls src/", listDir(t, filepath.Join(repo, "src")), 2.2)

	out := filepath.Join("..", "..", "docs", "demo.cast")
	if err := os.WriteFile(out, []byte(cast.b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d frames)", out, cast.frames)
}

// castWriter accumulates an asciicast v2 file.
type castWriter struct {
	b      strings.Builder
	at     float64
	frames int
}

func (c *castWriter) header(cols, rows int) {
	h, _ := json.Marshal(map[string]any{"version": 2, "width": cols, "height": rows})
	c.b.Write(h)
	c.b.WriteByte('\n')
}

func (c *castWriter) write(text string) {
	ev, _ := json.Marshal([]any{c.at, "o", text})
	c.b.Write(ev)
	c.b.WriteByte('\n')
}

// frame clears the screen and draws one view, then holds for hold seconds.
func (c *castWriter) frame(view string, hold float64) {
	c.write("\x1b[2J\x1b[H" + strings.ReplaceAll(view, "\n", "\r\n"))
	c.at += hold
	c.frames++
}

// shell draws a prompt, a typed command, and its real output.
func (c *castWriter) shell(dir, cmd, output string, hold float64) {
	c.write("\x1b[2J\x1b[H$ ")
	for _, r := range cmd {
		c.at += 0.045
		c.write(string(r))
	}
	c.at += 0.25
	c.write("\r\n" + strings.ReplaceAll(output, "\n", "\r\n") + "\r\n$ ")
	c.at += hold
	c.frames++
}

func listDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strings.Join(names, "  ")
}

// buildDemoRepo stages the session the demo replays: three agent turns, the
// last of which deleted the lexer and gutted the parser.
func buildDemoRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	write(".gitignore", "node_modules/\n.env\n")
	write("node_modules/dep.js", "installed\n")
	write("src/parser.go", parserV1)
	git("add", "-A")
	git("-c", "user.email=dev@example.com", "-c", "user.name=dev", "commit", "-qm", "initial parser")

	tl, err := timeline.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := func(label string) {
		t.Helper()
		if _, _, err := tl.Snap(label); err != nil {
			t.Fatal(err)
		}
	}

	write("src/lexer.go", lexerV1)
	snap("turn 4: add the lexer")

	write("src/parser.go", parserV2)
	snap("turn 5: wire up the parser")

	if err := os.Remove(filepath.Join(dir, "src", "lexer.go")); err != nil {
		t.Fatal(err)
	}
	write("src/parser.go", parserBroken)
	snap("turn 6: refactor error handling")
}

const parserV1 = `package parse

// Parse turns a token stream into an expression tree.
func Parse(tokens []Token) (*Node, error) {
	p := &parser{tokens: tokens}
	return p.expression()
}
`

const parserV2 = `package parse

// Parse turns a token stream into an expression tree.
func Parse(tokens []Token) (*Node, error) {
	p := &parser{tokens: tokens}
	node, err := p.expression()
	if err != nil {
		return nil, err
	}
	return node, p.expectEOF()
}
`

const lexerV1 = `package parse

// Lex splits source text into tokens.
func Lex(src string) []Token {
	var out []Token
	for _, line := range splitLines(src) {
		out = append(out, tokenize(line)...)
	}
	return out
}

func tokenize(line string) []Token { return scan(line) }
`

const parserBroken = `package parse

func Parse(tokens []Token) (*Node, error) {
	p := &parser{tokens: tokens}
	return p.expr()
}
`
