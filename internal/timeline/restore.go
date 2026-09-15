package timeline

import (
	"errors"
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
	OpSkip   Op = "skip"   // git ignores the path now, so scrubline leaves it alone
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
	// Restoring it undoes this restore. Empty when nothing was written.
	Safety Snapshot
	// SafetyCreated is false when the working tree was already captured by the
	// tip of the timeline, in which case Safety is that existing snapshot.
	SafetyCreated bool
	// Partial is set when writing began and then failed, so the working tree
	// matches neither the snapshot nor what the user had before.
	Partial bool
}

// ErrPathNotFound reports a --path selector that names nothing scrubline knows
// about. It is deliberately distinct from an empty plan: "I could not find that
// file" and "that file is already up to date" are different answers.
var ErrPathNotFound = errors.New("no such path")

// Problem is one reason a restore was refused before it wrote anything.
type Problem struct {
	Path   string
	Reason string
}

// PreflightError reports that the plan cannot be applied safely. Nothing has
// been written and no safety snapshot was taken.
type PreflightError struct {
	Problems []Problem
}

func (e *PreflightError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to restore, nothing was changed:\n")
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "  %s: %s\n", p.Path, p.Reason)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Restore materializes a snapshot into the working tree.
//
// The order matters and is the whole safety story. The plan is computed and
// checked before anything is written: if any path cannot be restored, the
// restore is refused with the working tree untouched and no safety snapshot
// minted, so retrying does not fill the timeline with junk. Only once the plan
// is known to be applicable is the current state snapshotted, and only then is
// anything written.
//
// Paths that git ignores right now are never written and never deleted.
func (t *Timeline) Restore(target Snapshot, opts RestoreOptions) (RestoreResult, error) {
	var res RestoreResult

	// One lock for the whole operation, so a concurrent `watch` cannot snapshot
	// a half-written tree or stage against the index while it is being used.
	unlock, err := t.lock()
	if err != nil {
		return res, err
	}
	defer unlock()

	currentTree, err := t.StageTree()
	if err != nil {
		return res, err
	}

	plan, err := t.plan(currentTree, target.Tree, opts.Path)
	if err != nil {
		return res, err
	}

	// A selector that matches nothing at all is an error, not a no-op.
	if opts.Path != "" && len(plan) == 0 {
		known, err := t.pathKnown(opts.Path, target.Tree, currentTree)
		if err != nil {
			return res, err
		}
		if !known {
			return res, fmt.Errorf("%w: %q is not in snapshot %s or the working tree "+
				"(paths are case-sensitive, and %s does not track files git ignores)",
				ErrPathNotFound, opts.Path, target.ShortID(), meta.Name)
		}
	}

	// The ignore rule, applied symmetrically: every entry in the plan is
	// checked, writes and deletes alike, so a path the user ignores now is
	// never touched in either direction.
	paths := make([]string, 0, len(plan))
	for _, c := range plan {
		paths = append(paths, c.Path)
	}
	ignored, err := t.Repo.CheckIgnore(paths)
	if err != nil {
		return res, err
	}
	for i, c := range plan {
		if ignored[c.Path] {
			plan[i].Op = OpSkip
		}
	}
	res.Plan = plan

	if opts.DryRun || countActionable(plan) == 0 {
		return res, nil
	}

	if problems := t.preflight(plan); len(problems) > 0 {
		return res, &PreflightError{Problems: problems}
	}

	safety, created, err := t.snapLocked(fmt.Sprintf("safety: before restore of %s", target.ShortID()))
	if err != nil {
		return res, fmt.Errorf("taking safety snapshot: %w", err)
	}
	res.Safety, res.SafetyCreated = safety, created

	if err := t.apply(target.Tree, plan); err != nil {
		res.Partial = true
		return res, err
	}
	return res, nil
}

// CountActionable is the number of entries a restore would act on, ignoring
// paths it has decided to leave alone.
func CountActionable(plan []Change) int { return countActionable(plan) }

func countActionable(plan []Change) int {
	n := 0
	for _, c := range plan {
		if c.Op != OpSkip {
			n++
		}
	}
	return n
}

// pathKnown reports whether a --path selector names anything in either tree.
func (t *Timeline) pathKnown(sel string, trees ...string) (bool, error) {
	clean := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(sel)), "/")
	for _, tree := range trees {
		out, err := t.Repo.GitRaw("ls-tree", "-r", "-z", "--name-only", tree, "--", clean)
		if err != nil {
			return false, err
		}
		if len(splitNUL(out)) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// preflight looks for every reason the plan cannot be applied, before a single
// byte is written. Collecting all of them means the user fixes their tree once
// rather than discovering the next collision on the next attempt.
func (t *Timeline) preflight(plan []Change) []Problem {
	var problems []Problem
	for _, c := range plan {
		abs := filepath.Join(t.Repo.Root, filepath.FromSlash(c.Path))
		switch c.Op {
		case OpSkip:
			continue

		case OpDelete:
			info, err := os.Lstat(abs)
			if err != nil {
				continue // already gone; the delete will be a no-op
			}
			if info.IsDir() {
				if !isEmptyDir(abs) {
					problems = append(problems, Problem{c.Path,
						"is a directory now and is not empty, so removing it could take files with it"})
				}
				continue
			}
			if p, ok := unwritableParent(t.Repo.Root, abs); ok {
				problems = append(problems, Problem{c.Path, "cannot be removed, " + p + " is not writable"})
			}

		case OpAdd, OpModify:
			// An ancestor that is a file cannot also be a directory.
			if bad, ok := fileInTheWay(t.Repo.Root, c.Path); ok {
				problems = append(problems, Problem{c.Path,
					"cannot be written because " + bad + " is a file, not a directory"})
				continue
			}
			info, err := os.Lstat(abs)
			if err == nil {
				if info.IsDir() && !isEmptyDir(abs) {
					// Restoring this path means replacing a directory with a
					// file. Emptying the directory is never done automatically:
					// it may hold ignored files that are not ours to delete.
					problems = append(problems, Problem{c.Path,
						"is a directory now and is not empty, so it cannot be replaced by a file"})
					continue
				}
				if !info.IsDir() && info.Mode().Perm()&0o200 == 0 {
					problems = append(problems, Problem{c.Path, "is read-only"})
					continue
				}
			}
			if p, ok := unwritableParent(t.Repo.Root, abs); ok {
				problems = append(problems, Problem{c.Path, "cannot be written, " + p + " is not writable"})
			}
		}
	}
	sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
	return problems
}

// fileInTheWay returns the first ancestor directory of path that exists as
// something other than a directory.
func fileInTheWay(root, path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := 0; i < len(parts)-1; i++ {
		ancestor := filepath.Join(root, filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		info, err := os.Lstat(ancestor)
		if err != nil {
			return "", false // does not exist yet, so it will be created
		}
		if !info.IsDir() {
			return strings.Join(parts[:i+1], "/"), true
		}
	}
	return "", false
}

// unwritableParent reports the nearest existing parent directory if it is not
// writable. The check is the owner write bit, which catches the read-only
// directory case without pretending to model every permission system.
func unwritableParent(root, abs string) (string, bool) {
	dir := filepath.Dir(abs)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if info.Mode().Perm()&0o200 == 0 {
				rel, relErr := filepath.Rel(root, dir)
				if relErr != nil || strings.HasPrefix(rel, "..") {
					rel = dir
				}
				return rel, true
			}
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir || len(dir) <= len(root) {
			return "", false
		}
		dir = parent
	}
}

func isEmptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
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

	// Read every blob through one git process rather than one per file.
	oids := make([]string, 0, len(wanted))
	seen := make(map[string]bool, len(wanted))
	for _, path := range wanted {
		e, ok := entries[path]
		if !ok || e.Mode == "160000" || seen[e.OID] {
			continue
		}
		seen[e.OID] = true
		oids = append(oids, e.OID)
	}
	blobs, err := t.Repo.CatFileBatch(oids)
	if err != nil {
		return err
	}

	for _, c := range plan {
		abs := filepath.Join(t.Repo.Root, filepath.FromSlash(c.Path))
		switch c.Op {
		case OpSkip:
			continue
		case OpDelete:
			if err := remove(abs); err != nil {
				return fmt.Errorf("removing %s: %w", c.Path, err)
			}
			pruneEmptyDirs(t.Repo.Root, filepath.Dir(abs))
		case OpAdd, OpModify:
			e, ok := entries[c.Path]
			if !ok {
				return fmt.Errorf("%s is missing from snapshot tree %s", c.Path, targetTree)
			}
			if err := writeEntry(abs, c.Path, e, blobs[e.OID]); err != nil {
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

// treeEntries looks up the mode and object id of each wanted path.
//
// Passing every path as a pathspec is fine for a handful and fatal for tens of
// thousands: 30,000 paths is about 2MB of argv and git fails to exec. Past a
// small number the whole tree is listed once and filtered here instead, which
// is both cheaper and immune to the argument limit.
func (t *Timeline) treeEntries(tree string, paths []string) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry, len(paths))
	if len(paths) == 0 {
		return entries, nil
	}

	args := []string{"ls-tree", "-r", "-z", tree}
	const pathspecLimit = 100
	var wanted map[string]bool
	if len(paths) <= pathspecLimit {
		args = append(args, "--")
		args = append(args, paths...)
	} else {
		wanted = make(map[string]bool, len(paths))
		for _, p := range paths {
			wanted[p] = true
		}
	}

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
		if wanted != nil && !wanted[path] {
			continue
		}
		entries[path] = treeEntry{Mode: fields[0], OID: fields[2]}
	}
	return entries, nil
}

func writeEntry(abs, path string, e treeEntry, content []byte) error {
	if e.Mode == "160000" {
		// A submodule pointer; restoring it would mean recursing into another
		// repository, which scrubline deliberately does not do.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("creating directory for %s: %w", path, err)
	}
	// Replace rather than truncate: the existing entry may be a symlink, or a
	// file whose mode differs from the one in the snapshot.
	if err := remove(abs); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	if e.Mode == "120000" {
		if err := os.Symlink(string(content), abs); err != nil {
			return fmt.Errorf("restoring symlink %s: %w", path, err)
		}
		return nil
	}
	mode := os.FileMode(0o644)
	if e.Mode == "100755" {
		mode = 0o755
	}
	if err := os.WriteFile(abs, content, mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// remove deletes a single entry. It never recurses: a directory in the way is
// removed only when it is empty, because anything inside it might be a file
// git ignores, which is not scrubline's to delete.
func remove(abs string) error {
	err := os.Remove(abs)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
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
			fmt.Fprintf(&b, "  skip    %s (git ignores it now)\n", c.Path)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
