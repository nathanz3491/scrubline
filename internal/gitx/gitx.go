// Package gitx is a thin wrapper over the git command line. scrubline stores
// snapshots as ordinary git objects, so every operation here is plumbing:
// nothing in this package touches the user's index, HEAD, or branches.
package gitx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
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

// GitWithIndexRaw is GitWithIndex without trimming, for NUL-separated output.
func (r *Repo) GitWithIndexRaw(indexPath string, args ...string) (string, error) {
	return runRaw(r.Root, []string{"GIT_INDEX_FILE=" + indexPath}, args...)
}

// GitWithIndexStdin feeds input to a git command run against an alternate index.
// Paths go in on stdin rather than argv so that a repository with tens of
// thousands of ignored files cannot overflow the argument list.
func (r *Repo) GitWithIndexStdin(indexPath, input string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Root
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CatFileBatchFunc reads many blobs through a single git process, handing each
// one to fn as it arrives.
//
// Two properties matter here. One git process instead of one per file: forking
// `git cat-file` per blob costs milliseconds each, so a few thousand files took
// tens of seconds, which behind a keypress in the browser reads as a hang.
// And blobs are streamed rather than collected: holding them all would make a
// restore's memory scale with the total size of the change set instead of with
// its largest single file, so restoring a few GB of assets would try to
// allocate a few GB. An OOM kill lands mid-write, which is exactly the
// partly-rewritten tree the pre-flight work exists to prevent -- and the
// process dies before it can report the safety snapshot.
//
// fn must not retain content: the buffer is only valid for the call.
func (r *Repo) CatFileBatchFunc(oids []string, fn func(oid string, content []byte) error) error {
	if len(oids) == 0 {
		return nil
	}

	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = r.Root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	// Requests go out on their own goroutine: git blocks writing responses once
	// the pipe fills, so writing every request up front would deadlock against
	// a reader that is busy writing files.
	writeErr := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(stdin)
		for _, oid := range oids {
			if _, err := w.WriteString(oid + "\n"); err != nil {
				stdin.Close()
				writeErr <- err
				return
			}
		}
		err := w.Flush()
		stdin.Close()
		writeErr <- err
	}()

	abort := func(err error) error {
		// Stop git rather than draining a response stream nobody will read.
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
		<-writeErr
		return err
	}

	reader := bufio.NewReader(stdout)
	for range oids {
		header, err := reader.ReadString('\n')
		if err != nil {
			return abort(fmt.Errorf("git cat-file --batch: %w (%s)", err, strings.TrimSpace(stderr.String())))
		}
		fields := strings.Fields(strings.TrimSpace(header))
		if len(fields) < 3 {
			return abort(fmt.Errorf("git cat-file --batch: unexpected response %q", strings.TrimSpace(header)))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return abort(fmt.Errorf("git cat-file --batch: bad size %q", fields[2]))
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return abort(fmt.Errorf("git cat-file --batch: reading %s: %w", fields[0], err))
		}
		// Each record is followed by a newline that is not part of the content.
		if _, err := reader.Discard(1); err != nil {
			return abort(err)
		}
		if err := fn(fields[0], buf); err != nil {
			return abort(err)
		}
	}

	if err := <-writeErr; err != nil {
		cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git cat-file --batch: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// GitRaw runs a git command and returns stdout without trimming, for output
// that is NUL-separated or otherwise whitespace-significant.
func (r *Repo) GitRaw(args ...string) (string, error) {
	out, err := runRaw(r.Root, nil, args...)
	return out, err
}

// CommitTree writes a commit object with the given tree and optional parent.
//
// The identity is fixed rather than taken from the user's git config: snapshots
// are private to the machine and never pushed, and a user who has not set
// user.email should still be able to record their work.
func (r *Repo) CommitTree(tree, parent, message string) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", message)
	return run(r.Root, identityEnv, args...)
}

// CommitTreeAt is CommitTree with an explicit timestamp, so that rewriting the
// timeline preserves when each snapshot was actually taken.
func (r *Repo) CommitTreeAt(tree, parent, message string, when time.Time) (string, error) {
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	args = append(args, "-m", message)
	stamp := when.Format(time.RFC3339)
	env := append([]string{}, identityEnv...)
	env = append(env, "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	return run(r.Root, env, args...)
}

var identityEnv = []string{
	"GIT_AUTHOR_NAME=scrubline",
	"GIT_AUTHOR_EMAIL=scrubline@localhost",
	"GIT_COMMITTER_NAME=scrubline",
	"GIT_COMMITTER_EMAIL=scrubline@localhost",
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
