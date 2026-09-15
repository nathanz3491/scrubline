package timeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nathanz3491/scrubline/internal/meta"
)

// Op is what restoring will do to one path.
type Op string

const (
	OpAdd    Op = "add"    // the path is in the snapshot but not in the working tree
	OpModify Op = "modify" // the path differs between the two
	OpDelete Op = "delete" // the path is in the working tree but not in the snapshot
	OpSkip   Op = "skip"   // the path would be written, but git now ignores it
)

// Change is one entry in a restore plan.
type Change struct {
	Op   Op
	Path string
}

// RestoreOptions controls a restore.
type RestoreOptions struct {
	// DryRun computes and returns the plan without touching the working tree
	// and without taking a safety snapshot.
	DryRun bool
	// Path limits the restore to one file, or to one directory and everything
	// under it. Empty means the whole tree.
	Path string
}

// RestoreResult reports what a restore did.
type RestoreResult struct {
	Plan []Change
	// Safety is the snapshot of the working tree as it was before the restore.
	// Restoring it undoes this restore. Empty only for a dry run.
	Safety Snapshot
	// SafetyCreated is false when the working tree was already captured by the
	// tip of the timeline, in which case Safety is that existing snapshot.
	SafetyCreated bool
}

// Restore materializes a snapshot into the working tree.
//
// Before anything is written, the current state of the working tree is captured
// as a snapshot, so that every restore is itself undoable. Paths that git
// currently ignores are never written and never deleted.
func (t *Timeline) Restore(target Snapshot, opts RestoreOptions) (RestoreResult, error) {
	var res RestoreResult

	currentTree := ""
	if opts.DryRun {
		tree, err := t.StageTree()
		if err != nil {
			return res, err
		}
		currentTree = tree
	} else {
		safety, created, err := t.Snap(fmt.Sprintf("safety: before restore of %s", target.ShortID()))
		if err != nil {
			return res, fmt.Errorf("taking safety snapshot: %w", err)
		}
		res.Safety, res.SafetyCreated = safety, created
		currentTree = safety.Tree
	}

	plan, err := t.plan(currentTree, target.Tree, opts.Path)
	if err != nil {
		return res, err
	}

	// A path that was snapshotted before the user ignored it must not be written
	// back over whatever lives there now.
	var writes []string
	for _, c := range plan {
		if c.Op == OpAdd || c.Op == OpModify {
			writes = append(writes, c.Path)
		}
	}
	ignored, err := t.Repo.CheckIgnore(writes)
	if err != nil {
		return res, err
	}
	for i, c := range plan {
		if ignored[c.Path] {
			plan[i].Op = OpSkip
		}
	}
	res.Plan = plan

	if opts.DryRun {
		return res, nil
	}
	if err := t.apply(target.Tree, plan); err != nil {
		return res, err
	}
	return res, nil
}

func (t *Timeline) plan(currentTree, targetTree, only string) ([]Change, error) {
	if currentTree == targetTree {
		return nil, nil
	}
	out, err := t.Repo.GitRaw("diff", "--name-status", "-z", currentTree, targetTree)
	if err != nil {
		return nil, err
	}
	// -z output is NUL-separated alternating status and path.
	fields := splitNUL(out)
	var plan []Change
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i], fields[i+1]
		if !matchesPath(path, only) {
			continue
		}
		var op Op
		switch {
		case strings.HasPrefix(status, "A"):
			op = OpAdd
		case strings.HasPrefix(status, "D"):
			op = OpDelete
		default:
			op = OpModify
		}
		plan = append(plan, Change{Op: op, Path: path})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].Path < plan[j].Path })
	return plan, nil
}

// matchesPath reports whether path is covered by a --path selector, which may
// name a file or a directory.
func matchesPath(path, only string) bool {
	if only == "" {
		return true
	}
	only = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(only)), "/")
	return path == only || strings.HasPrefix(path, only+"/")
}

func (t *Timeline) apply(targetTree string, plan []Change) error {
	var wanted []string
	for _, c := range plan {
		if c.Op == OpAdd || c.Op == OpModify {
			wanted = append(wanted, c.Path)
		}
	}
	entries, err := t.treeEntries(targetTree, wanted)
	if err != nil {
		return err
	}

	for _, c := range plan {
		abs := filepath.Join(t.Repo.Root, filepath.FromSlash(c.Path))
		switch c.Op {
		case OpSkip:
			continue
		case OpDelete:
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s: %w", c.Path, err)
			}
			pruneEmptyDirs(t.Repo.Root, filepath.Dir(abs))
		case OpAdd, OpModify:
			e, ok := entries[c.Path]
			if !ok {
				return fmt.Errorf("%s is missing from snapshot tree %s", c.Path, targetTree)
			}
			if err := t.writeEntry(abs, c.Path, e); err != nil {
				return err
			}
		}
	}
	return nil
}

type treeEntry struct {
	Mode string
	OID  string
}

func (t *Timeline) treeEntries(tree string, paths []string) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)
	if len(paths) == 0 {
		return entries, nil
	}
	args := []string{"ls-tree", "-r", "-z", tree, "--"}
	args = append(args, paths...)
	out, err := t.Repo.GitRaw(args...)
	if err != nil {
		return nil, err
	}
	for _, rec := range splitNUL(out) {
		// "<mode> <type> <oid>\t<path>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields, path := strings.Fields(rec[:tab]), rec[tab+1:]
		if len(fields) < 3 {
			continue
		}
		entries[path] = treeEntry{Mode: fields[0], OID: fields[2]}
	}
	return entries, nil
}

func (t *Timeline) writeEntry(abs, path string, e treeEntry) error {
	if e.Mode == "160000" {
		// A submodule pointer; restoring it would mean recursing into another
		// repository, which scrubline deliberately does not do.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	content, err := t.Repo.GitRaw("cat-file", "blob", e.OID)
	if err != nil {
		return fmt.Errorf("reading %s from snapshot: %w", path, err)
	}
	// Replace rather than truncate: the existing entry may be a symlink, or a
	// file whose mode differs from the one in the snapshot.
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	if e.Mode == "120000" {
		if err := os.Symlink(content, abs); err != nil {
			return fmt.Errorf("restoring symlink %s: %w", path, err)
		}
		return nil
	}
	mode := os.FileMode(0o644)
	if e.Mode == "100755" {
		mode = 0o755
	}
	if err := os.WriteFile(abs, []byte(content), mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// pruneEmptyDirs removes directories left empty by a delete, stopping at the
// worktree root. os.Remove fails on a non-empty directory, so a directory that
// still holds ignored files is left alone.
func pruneEmptyDirs(root, dir string) {
	for {
		if dir == root || !strings.HasPrefix(dir, root) {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// Describe renders a plan for humans.
func Describe(plan []Change) string {
	if len(plan) == 0 {
		return fmt.Sprintf("the working tree already matches this snapshot, %s would change nothing", meta.Name)
	}
	var b strings.Builder
	for _, c := range plan {
		switch c.Op {
		case OpAdd:
			fmt.Fprintf(&b, "  add     %s\n", c.Path)
		case OpModify:
			fmt.Fprintf(&b, "  modify  %s\n", c.Path)
		case OpDelete:
			fmt.Fprintf(&b, "  delete  %s\n", c.Path)
		case OpSkip:
			fmt.Fprintf(&b, "  skip    %s (now ignored by git)\n", c.Path)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
