// Package timeline implements the snapshot store.
//
// A snapshot is an ordinary git commit on a hidden ref (refs/scrubline/timeline)
// whose tree is the current working tree. Snapshots are staged through a private
// index file, so the user's index, HEAD, and branches are never touched.
//
// The one invariant everything else depends on: a snapshot tree contains exactly
// the non-ignored files in the working tree. The private index is never seeded
// from HEAD, so a path ignored by git can never enter a snapshot -- which in turn
// means restore can never delete or overwrite one.
package timeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nathanz3491/scrubline/internal/gitx"
	"github.com/nathanz3491/scrubline/internal/meta"
)

// Timeline is the snapshot store for one repository.
type Timeline struct {
	Repo *gitx.Repo

	stateDir  string
	indexPath string
}

// Snapshot is one entry on the timeline.
type Snapshot struct {
	ID     string // full commit id
	Tree   string // tree id
	Parent string // parent snapshot id, empty for the first
	When   time.Time
	Branch string
	Label  string
	Files  int // files changed relative to the previous snapshot
}

// ShortID is the id as displayed and accepted on the command line.
func (s Snapshot) ShortID() string { return gitx.Short(s.ID) }

// Open prepares the snapshot store for the repository containing dir.
func Open(dir string) (*Timeline, error) {
	repo, err := gitx.Discover(dir)
	if err != nil {
		return nil, err
	}
	t := &Timeline{Repo: repo}
	t.stateDir = filepath.Join(repo.GitDir, meta.StateDir)
	t.indexPath = filepath.Join(t.stateDir, "index")
	if err := os.MkdirAll(t.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s state directory: %w", meta.Name, err)
	}
	return t, nil
}

// StageTree records the current working tree as a git tree object and returns
// its id. Nothing is committed and no user-visible git state changes.
//
// The index file is kept between calls purely as a stat cache: it is what keeps
// `watch` cheap on a large repository, because git only re-hashes files whose
// stat information changed.
func (t *Timeline) StageTree() (string, error) {
	if _, err := t.Repo.GitWithIndex(t.indexPath, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	return t.Repo.GitWithIndex(t.indexPath, "write-tree")
}

// Tip returns the most recent snapshot, or ok=false when the timeline is empty.
func (t *Timeline) Tip() (Snapshot, bool, error) {
	if !t.Repo.RefExists(meta.TimelineRef) {
		return Snapshot{}, false, nil
	}
	snaps, err := t.List(1)
	if err != nil || len(snaps) == 0 {
		return Snapshot{}, false, err
	}
	return snaps[0], true, nil
}

// Snap captures the working tree. When the tree is byte-identical to the most
// recent snapshot, no new snapshot is created and created=false is returned
// along with the existing tip -- so callers always get a usable id back.
//
// A label passed here takes precedence over a label left pending by `mark`.
func (t *Timeline) Snap(label string) (snap Snapshot, created bool, err error) {
	unlock, err := t.lock()
	if err != nil {
		return Snapshot{}, false, err
	}
	defer unlock()

	tree, err := t.StageTree()
	if err != nil {
		return Snapshot{}, false, err
	}

	tip, hasTip, err := t.Tip()
	if err != nil {
		return Snapshot{}, false, err
	}
	if hasTip && tip.Tree == tree {
		return tip, false, nil
	}

	if label == "" {
		label = t.takePendingLabel()
	}

	base, err := t.baseline(tip, hasTip)
	if err != nil {
		return Snapshot{}, false, err
	}
	changed, err := t.diffNames(base, tree)
	if err != nil {
		return Snapshot{}, false, err
	}

	parent := ""
	if hasTip {
		parent = tip.ID
	}
	id, err := t.Repo.CommitTree(tree, parent, buildMessage(label, t.Repo.CurrentBranch(), len(changed)))
	if err != nil {
		return Snapshot{}, false, err
	}
	if _, err := t.Repo.Git("update-ref", meta.TimelineRef, id); err != nil {
		return Snapshot{}, false, err
	}

	return Snapshot{
		ID:     id,
		Tree:   tree,
		Parent: parent,
		When:   time.Now(),
		Branch: t.Repo.CurrentBranch(),
		Label:  label,
		Files:  len(changed),
	}, true, nil
}

// List returns snapshots newest first. A limit of 0 or less returns all of them.
func (t *Timeline) List(limit int) ([]Snapshot, error) {
	if !t.Repo.RefExists(meta.TimelineRef) {
		return nil, nil
	}
	args := []string{"log", "--format=%H%x1f%T%x1f%P%x1f%ct%x1f%B%x1e"}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	args = append(args, meta.TimelineRef)
	out, err := t.Repo.GitRaw(args...)
	if err != nil {
		return nil, err
	}

	var snaps []Snapshot
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		fields := strings.SplitN(rec, "\x1f", 5)
		if len(fields) < 5 {
			continue
		}
		secs, _ := strconv.ParseInt(fields[3], 10, 64)
		label, branch, files := parseMessage(fields[4])
		snaps = append(snaps, Snapshot{
			ID:     fields[0],
			Tree:   fields[1],
			Parent: firstParent(fields[2]),
			When:   time.Unix(secs, 0),
			Branch: branch,
			Label:  label,
			Files:  files,
		})
	}
	return snaps, nil
}

// ErrNoSnapshot reports a revision that does not name a snapshot on the timeline.
var ErrNoSnapshot = errors.New("no such snapshot")

// Lookup resolves a user-supplied id. "latest" and "last" name the newest
// snapshot; anything else must resolve to a commit that is on the timeline, so
// that an arbitrary repository commit cannot be passed to restore by mistake.
func (t *Timeline) Lookup(rev string) (Snapshot, error) {
	snaps, err := t.List(0)
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("%w: the timeline is empty, run `%s snap` first", ErrNoSnapshot, meta.Name)
	}
	// An empty id is refused rather than treated as "latest": the realistic
	// source of one is an unset variable in a script or hook, and resolving it
	// to a snapshot would let `restore "$SNAP"` rewrite the tree by accident.
	if strings.TrimSpace(rev) == "" {
		return Snapshot{}, fmt.Errorf("%w: empty snapshot id", ErrNoSnapshot)
	}
	switch rev {
	case "latest", "last":
		return snaps[0], nil
	}

	full, err := t.Repo.Resolve(rev)
	if err != nil || full == "" {
		// Fall back to prefix matching so short ids from `list` always work.
		var hits []Snapshot
		for _, s := range snaps {
			if strings.HasPrefix(s.ID, rev) {
				hits = append(hits, s)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
		if len(hits) > 1 {
			return Snapshot{}, fmt.Errorf("%q is ambiguous, it matches %d snapshots", rev, len(hits))
		}
		return Snapshot{}, fmt.Errorf("%w: %q", ErrNoSnapshot, rev)
	}
	for _, s := range snaps {
		if s.ID == full {
			return s, nil
		}
	}
	return Snapshot{}, fmt.Errorf("%w: %q is a commit, but not one on the %s timeline", ErrNoSnapshot, rev, meta.Name)
}

// Files lists the paths a snapshot changed relative to the snapshot before it.
func (t *Timeline) Files(s Snapshot) ([]string, error) {
	var base string
	var err error
	if s.Parent != "" {
		base, err = t.Repo.Git("rev-parse", s.Parent+"^{tree}")
		if err != nil {
			return nil, err
		}
	} else {
		base, err = t.baseline(Snapshot{}, false)
		if err != nil {
			return nil, err
		}
	}
	return t.diffNames(base, s.Tree)
}

// SetPendingLabel records a label for the next snapshot that gets created.
func (t *Timeline) SetPendingLabel(label string) error {
	return os.WriteFile(filepath.Join(t.stateDir, "pending-label"), []byte(label), 0o644)
}

func (t *Timeline) takePendingLabel() string {
	path := filepath.Join(t.stateDir, "pending-label")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	os.Remove(path)
	return strings.TrimSpace(string(b))
}

// baseline is what the first snapshot's change count is measured against: the
// HEAD tree when the repository has commits, the empty tree when it does not.
func (t *Timeline) baseline(tip Snapshot, hasTip bool) (string, error) {
	if hasTip {
		return tip.Tree, nil
	}
	if t.Repo.HasHead() {
		return t.Repo.Git("rev-parse", "HEAD^{tree}")
	}
	return t.Repo.Git("hash-object", "-t", "tree", "/dev/null")
}

func (t *Timeline) diffNames(from, to string) ([]string, error) {
	if from == to {
		return nil, nil
	}
	out, err := t.Repo.GitRaw("diff", "--name-only", "-z", from, to)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// lock serializes snapshot creation between concurrent scrubline processes --
// a `watch` in one terminal and a manual `snap` in another.
func (t *Timeline) lock() (func(), error) {
	path := filepath.Join(t.stateDir, "lock")
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("creating lock file: %w", err)
		}
		if attempt >= 50 {
			return nil, fmt.Errorf("another %s process is holding %s; remove it if no %s is running", meta.Name, path, meta.Name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func buildMessage(label, branch string, files int) string {
	subject := label
	if subject == "" {
		subject = "snapshot"
	}
	// Keep the subject to one line: a label with newlines would corrupt parsing.
	subject = strings.ReplaceAll(subject, "\n", " ")
	return fmt.Sprintf("%s\n\nScrubline-Branch: %s\nScrubline-Files: %d\nScrubline-Labelled: %t\n",
		subject, branch, files, label != "")
}

func parseMessage(body string) (label, branch string, files int) {
	lines := strings.Split(body, "\n")
	if len(lines) > 0 {
		label = strings.TrimSpace(lines[0])
	}
	labelled := false
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "Scrubline-Branch: "):
			branch = strings.TrimSpace(strings.TrimPrefix(ln, "Scrubline-Branch: "))
		case strings.HasPrefix(ln, "Scrubline-Files: "):
			files, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "Scrubline-Files: ")))
		case strings.HasPrefix(ln, "Scrubline-Labelled: "):
			labelled = strings.TrimSpace(strings.TrimPrefix(ln, "Scrubline-Labelled: ")) == "true"
		}
	}
	if !labelled {
		label = ""
	}
	return label, branch, files
}

func firstParent(parents string) string {
	parents = strings.TrimSpace(parents)
	if parents == "" {
		return ""
	}
	return strings.Fields(parents)[0]
}

func splitNUL(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
