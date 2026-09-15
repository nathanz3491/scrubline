package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHookOutputIsFreeOfEscapeCodes guards the contract that matters for agent
// hooks: when output is not a terminal, scrubline writes plain text and nothing
// else.
//
// It is worth a test rather than a comment because the risk comes from a
// dependency, not from this code. Linking bubbletea in for the browser makes
// the binary probe the terminal for its background colour at startup; that
// probe is gated on stdout being a terminal today, and a version bump could
// silently ungate it. A hook that pipes `scrubline mark` into a log would then
// start collecting escape sequences.
func TestHookOutputIsFreeOfEscapeCodes(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("needs the go tool to build the binary under test")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "scrubline")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building: %v\n%s", err, out)
	}

	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Each command runs with a changed tree so it has real work to report.
	commands := [][]string{
		{"snap"},
		{"mark", "turn 12: refactor"},
		{"list"},
		{"show", "latest"},
		{"show", "latest", "--files"},
		{"prune", "--older-than", "30d"},
		{"version"},
	}
	for i, args := range commands {
		content := []byte(strings.Repeat("edit\n", i+1))
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), content, 0o644); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(bin, args...)
		cmd.Dir = repo
		// Claim a terminal in the environment; only the pipes should matter.
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr.String())
		}
		for name, buf := range map[string]*bytes.Buffer{"stdout": &stdout, "stderr": &stderr} {
			if n := bytes.Count(buf.Bytes(), []byte{0x1b}); n != 0 {
				t.Errorf("%v wrote %d escape byte(s) to %s: %q", args, n, name, buf.String())
			}
		}
	}
}
