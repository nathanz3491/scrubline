// Package gitx is a thin wrapper over the git command line. scrubline stores
// snapshots as ordinary git objects, so every operation here is plumbing:
// nothing in this package touches the user's index, HEAD, or branches.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrNotARepo is returned by Discover when dir is not inside a git worktree.
var ErrNotARepo = errors.New("not a git repository")

// Repo is a git worktree plus the path to its git directory.
type Repo struct {
	Root   string // absolute path to the worktree root
	GitDir string // absolute path to the .git directory
}

// Discover locates the worktree containing dir.
func Discover(dir string) (*Repo, error) {
	root, err := run(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, ErrNotARepo
	}
	gitDir, err := run(dir, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, ErrNotARepo
	}
	return &Repo{Root: root, GitDir: gitDir}, nil
}

// Git runs a git command in the worktree root and returns trimmed stdout.
func (r *Repo) Git(args ...string) (string, error) {
	return run(r.Root, nil, args...)
}

// GitWithIndex runs a git command against an alternate index file, leaving the
// user's real index untouched. This is how snapshots are staged.
func (r *Repo) GitWithIndex(indexPath string, args ...string) (string, error) {
	return run(r.Root, []string{"GIT_INDEX_FILE=" + indexPath}, args...)
}

// GitRaw runs a git command and returns stdout without trimming, for output
// that is NUL-separated or otherwise whitespace-significant.
func (r *Repo) GitRaw(args ...string) (string, error) {
	out, err := runRaw(r.Root, nil, args...)
	return out, err
}

// CommitTree writes a commit object with the given tree and optional parent.
func (r *Repo) CommitTree(tree, parent, message string) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", message)
	return r.Git(args...)
}

// Resolve turns a revision string into a full object id.
func (r *Repo) Resolve(rev string) (string, error) {
	return r.Git("rev-parse", "--verify", "--quiet", rev+"^{commit}")
}

// RefExists reports whether ref points at an object.
func (r *Repo) RefExists(ref string) bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// HasHead reports whether HEAD points at a commit. A freshly initialized
// repository with no commits does not.
func (r *Repo) HasHead() bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	return err == nil
}

// CurrentBranch returns the checked-out branch name, or "HEAD" when detached.
func (r *Repo) CurrentBranch() string {
	out, err := r.Git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out == "" {
		return "HEAD"
	}
	return out
}

// Short abbreviates an object id for display.
func Short(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

func run(dir string, env []string, args ...string) (string, error) {
	out, err := runRaw(dir, env, args...)
	return strings.TrimRight(out, "\n"), err
}

func runRaw(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// CheckIgnore returns the subset of paths that git currently ignores.
//
// `git check-ignore` exits 1 to mean "none of these are ignored", which is not
// an error condition, so this cannot go through the normal helpers.
func (r *Repo) CheckIgnore(paths []string) (map[string]bool, error) {
	ignored := make(map[string]bool)
	if len(paths) == 0 {
		return ignored, nil
	}
	cmd := exec.Command("git", "check-ignore", "-z", "--stdin")
	cmd.Dir = r.Root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("git check-ignore: %s", strings.TrimSpace(stderr.String()))
		}
	}
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	return ignored, nil
}
