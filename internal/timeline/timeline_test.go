package timeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// testRepo is a throwaway git repository with scrubline opened on it.
type testRepo struct {
	t   *testing.T
	dir string
	tl  *Timeline
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir := t.TempDir()
	// macOS temp dirs are symlinked through /private; resolve so that paths
	// compare equal to what git reports.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	r := &testRepo{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	tl, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.tl = tl
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) write(path, content string) {
	r.t.Helper()
	abs := filepath.Join(r.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) read(path string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(path)))
	if err != nil {
		r.t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func (r *testRepo) remove(path string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, filepath.FromSlash(path))); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(r.dir, filepath.FromSlash(path)))
	return err == nil
}

func (r *testRepo) snap(label string) Snapshot {
	r.t.Helper()
	s, _, err := r.tl.Snap(label)
	if err != nil {
		r.t.Fatalf("Snap: %v", err)
	}
	return s
}

// state fingerprints every non-ignored file, for byte-identity assertions.
func (r *testRepo) state() map[string]string {
	r.t.Helper()
	out := map[string]string{}
	err := filepath.Walk(r.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(r.dir, p)
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			if rel == ".git" || rel == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == ".env" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func sameState(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSnapshotExcludesIgnoredPathsAndIncludesUntracked(t *testing.T) {
	r := newTestRepo(t)
	r.write(".gitignore", "node_modules/\n.env\n")
	r.write("tracked.txt", "hello\n")
	r.git("add", "-A")
	r.git("-c", "user.email=t@t", "-c", "user.name=T", "commit", "-qm", "init")

	r.write("tracked.txt", "hello\nworld\n") // modified tracked
	r.write("untracked.txt", "new\n")        // untracked, not ignored
	r.write("node_modules/dep.js", "junk\n") // ignored directory
	r.write(".env", "SECRET=1\n")            // ignored file

	s := r.snap("")
	files, err := r.tl.Files(s)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	want := []string{"tracked.txt", "untracked.txt"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("snapshot files = %v, want %v", files, want)
	}
}

func TestSnapshotDoesNotDisturbGitStatus(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("-c", "user.email=t@t", "-c", "user.name=T", "commit", "-qm", "init")
	r.write("a.txt", "two\n")
	r.write("b.txt", "new\n")

	before := r.git("status", "--porcelain")
	head := r.git("rev-parse", "HEAD")
	r.snap("")
	if after := r.git("status", "--porcelain"); after != before {
		t.Fatalf("git status changed:\nbefore %q\nafter  %q", before, after)
	}
	if now := r.git("rev-parse", "HEAD"); now != head {
		t.Fatalf("HEAD moved from %s to %s", head, now)
	}
}

func TestSnapIsANoOpWhenNothingChanged(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")

	first, created, err := r.tl.Snap("")
	if err != nil || !created {
		t.Fatalf("first snap: created=%v err=%v", created, err)
	}
	second, created, err := r.tl.Snap("")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second snap created a snapshot despite no changes")
	}
	if second.ID != first.ID {
		t.Fatalf("no-op snap returned %s, want the existing tip %s", second.ID, first.ID)
	}
	snaps, err := r.tl.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("timeline has %d snapshots, want 1", len(snaps))
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	r := newTestRepo(t)
	r.write("src/a.txt", "one\n")
	r.write("src/b.txt", "two\n")
	r.write("deep/nested/c.txt", "three\n")
	good := r.snap("good")
	want := r.state()

	r.remove("src/a.txt")
	r.write("src/b.txt", "MANGLED\n")
	r.write("deep/nested/c.txt", "ALSO MANGLED\n")
	r.write("stray.txt", "stray\n")

	if _, err := r.tl.Restore(good, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := r.state(); !sameState(got, want) {
		t.Fatalf("working tree after restore = %v, want %v", got, want)
	}
}

func TestRestoreIsItselfUndoable(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "original\n")
	r.write("b.txt", "keep\n")
	good := r.snap("good")

	r.write("a.txt", "work in progress\n")
	r.remove("b.txt")
	r.write("c.txt", "new work\n")
	wrecked := r.state()

	res, err := r.tl.Restore(good, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Safety.ID == "" {
		t.Fatal("restore did not report a safety snapshot")
	}
	if sameState(r.state(), wrecked) {
		t.Fatal("restore did not change the working tree")
	}

	if _, err := r.tl.Restore(res.Safety, RestoreOptions{}); err != nil {
		t.Fatalf("restoring the safety snapshot: %v", err)
	}
	if got := r.state(); !sameState(got, wrecked) {
		t.Fatalf("undo left %v, want the pre-restore state %v", got, wrecked)
	}
}

func TestRestoreNeverTouchesIgnoredPaths(t *testing.T) {
	r := newTestRepo(t)
	r.write(".gitignore", "node_modules/\n.env\n")
	r.write("a.txt", "one\n")
	good := r.snap("")

	r.write("node_modules/dep.js", "installed\n")
	r.write(".env", "SECRET=1\n")
	r.write("a.txt", "mangled\n")

	if _, err := r.tl.Restore(good, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if !r.exists("node_modules/dep.js") || r.read("node_modules/dep.js") != "installed\n" {
		t.Fatal("restore deleted or modified an ignored path")
	}
	if !r.exists(".env") || r.read(".env") != "SECRET=1\n" {
		t.Fatal("restore deleted or modified .env")
	}
	if r.read("a.txt") != "one\n" {
		t.Fatal("restore failed to restore the non-ignored file")
	}
}

func TestRestoreSkipsPathThatBecameIgnored(t *testing.T) {
	r := newTestRepo(t)
	r.write("secrets.txt", "from the snapshot\n")
	good := r.snap("")

	// The user ignores the file after it was snapshotted, and puts something
	// else there. Restoring must not overwrite it.
	r.write(".gitignore", "secrets.txt\n")
	r.write("secrets.txt", "current, must survive\n")

	res, err := r.tl.Restore(good, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.read("secrets.txt") != "current, must survive\n" {
		t.Fatal("restore overwrote a path that is now ignored")
	}
	var skipped bool
	for _, c := range res.Plan {
		if c.Path == "secrets.txt" && c.Op == OpSkip {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("plan did not report the skip: %v", res.Plan)
	}
}

func TestRestoreDryRunChangesNothing(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	good := r.snap("")
	r.write("a.txt", "mangled\n")
	r.write("b.txt", "extra\n")

	before := r.state()
	statBefore, err := os.Stat(filepath.Join(r.dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.tl.Restore(good, RestoreOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan) != 2 {
		t.Fatalf("plan = %v, want 2 entries", res.Plan)
	}
	if !sameState(r.state(), before) {
		t.Fatal("dry run modified the working tree")
	}
	statAfter, err := os.Stat(filepath.Join(r.dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Fatal("dry run changed a file's modification time")
	}
	if res.SafetyCreated {
		t.Fatal("dry run created a snapshot")
	}
}

func TestRestoreSinglePath(t *testing.T) {
	r := newTestRepo(t)
	r.write("src/a.txt", "one\n")
	r.write("src/b.txt", "two\n")
	good := r.snap("")

	r.write("src/a.txt", "mangled a\n")
	r.write("src/b.txt", "mangled b\n")

	if _, err := r.tl.Restore(good, RestoreOptions{Path: "src/a.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := r.read("src/a.txt"); got != "one\n" {
		t.Fatalf("src/a.txt = %q, want the snapshot content", got)
	}
	if got := r.read("src/b.txt"); got != "mangled b\n" {
		t.Fatalf("src/b.txt = %q, want it left alone", got)
	}
}

func TestRestoreDirectoryPath(t *testing.T) {
	r := newTestRepo(t)
	r.write("src/a.txt", "one\n")
	r.write("docs/d.txt", "docs\n")
	good := r.snap("")
	r.write("src/a.txt", "mangled\n")
	r.write("docs/d.txt", "mangled docs\n")

	if _, err := r.tl.Restore(good, RestoreOptions{Path: "src"}); err != nil {
		t.Fatal(err)
	}
	if r.read("src/a.txt") != "one\n" {
		t.Fatal("directory restore did not restore its file")
	}
	if r.read("docs/d.txt") != "mangled docs\n" {
		t.Fatal("directory restore touched a path outside it")
	}
}

func TestRestorePreservesExecutableBitAndSymlinks(t *testing.T) {
	r := newTestRepo(t)
	r.write("script.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(r.dir, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("script.sh", filepath.Join(r.dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	good := r.snap("")

	r.remove("link")
	if err := os.Chmod(filepath.Join(r.dir, "script.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.write("script.sh", "mangled\n")

	if _, err := r.tl.Restore(good, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(r.dir, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("executable bit lost, mode = %v", info.Mode())
	}
	li, err := os.Lstat(filepath.Join(r.dir, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was not restored as a symlink")
	}
}

func TestLookupRejectsCommitsOutsideTheTimeline(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.git("-c", "user.email=t@t", "-c", "user.name=T", "commit", "-qm", "init")
	head := r.git("rev-parse", "HEAD")
	r.write("a.txt", "two\n")
	r.snap("")

	if _, err := r.tl.Lookup(head); err == nil {
		t.Fatal("Lookup accepted a commit that is not on the timeline")
	}
	if _, err := r.tl.Lookup("definitely-not-an-id"); err == nil {
		t.Fatal("Lookup accepted nonsense")
	}
}

func TestLookupResolvesShortIDsAndLatest(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	first := r.snap("")
	r.write("a.txt", "two\n")
	second := r.snap("")

	got, err := r.tl.Lookup(first.ShortID())
	if err != nil || got.ID != first.ID {
		t.Fatalf("short id lookup = %v, %v", got.ID, err)
	}
	got, err = r.tl.Lookup("latest")
	if err != nil || got.ID != second.ID {
		t.Fatalf("latest = %v, %v want %v", got.ID, err, second.ID)
	}
}

func TestPendingLabelIsAppliedToTheNextSnapshot(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.snap("")

	if err := r.tl.SetPendingLabel("turn 5"); err != nil {
		t.Fatal(err)
	}
	r.write("a.txt", "two\n")
	s := r.snap("")
	if s.Label != "turn 5" {
		t.Fatalf("label = %q, want %q", s.Label, "turn 5")
	}

	// The label is consumed, not sticky.
	r.write("a.txt", "three\n")
	next := r.snap("")
	if next.Label != "" {
		t.Fatalf("label leaked to the following snapshot: %q", next.Label)
	}
}

func TestWatchDebouncesABurstIntoOneSnapshot(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "start\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	snapshots := make(chan Snapshot, 16)
	done := make(chan error, 1)
	go func() {
		done <- r.tl.Watch(ctx, WatchOptions{Interval: 20 * time.Millisecond, Debounce: 200 * time.Millisecond}, func(ev WatchEvent) {
			if ev.Snapshot != nil {
				snapshots <- *ev.Snapshot
			}
		})
	}()

	// The recorder captures the starting state first; drain that.
	select {
	case <-snapshots:
	case <-time.After(2 * time.Second):
		t.Fatal("recorder never captured the initial state")
	}

	// Three edits in rapid succession must collapse into one snapshot.
	for _, content := range []string{"edit one\n", "edit two\n", "edit three\n"} {
		r.write("a.txt", content)
		time.Sleep(30 * time.Millisecond)
	}

	select {
	case s := <-snapshots:
		if s.Files != 1 {
			t.Fatalf("debounced snapshot touched %d files, want 1", s.Files)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recorder never snapshotted the burst")
	}

	// Allow time for any extra snapshots the debounce should have prevented.
	time.Sleep(400 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if extra := len(snapshots); extra != 0 {
		t.Fatalf("burst produced %d extra snapshots, want 1 in total", extra)
	}
	if r.read("a.txt") != "edit three\n" {
		t.Fatal("recorder altered the working tree")
	}
}

func TestWatchLeavesNoLockBehind(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.tl.Watch(ctx, WatchOptions{Interval: 20 * time.Millisecond, Debounce: 50 * time.Millisecond}, func(WatchEvent) {})
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(r.dir, ".git", "scrubline", "lock")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock file survived shutdown: %v", err)
	}
	// A subsequent snapshot must work immediately.
	r.write("a.txt", "two\n")
	if _, _, err := r.tl.Snap(""); err != nil {
		t.Fatalf("snapshot after watch: %v", err)
	}
}

func TestOpenRejectsANonRepository(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a directory that is not a git repository")
	}
}

func TestLookupRefusesABlankID(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.snap("")

	// The realistic source of a blank id is an unset variable in a script, so
	// it must not resolve to the newest snapshot.
	for _, blank := range []string{"", " ", "\t", "\n"} {
		if _, err := r.tl.Lookup(blank); err == nil {
			t.Fatalf("Lookup(%q) resolved instead of refusing", blank)
		}
	}
	// "latest" still works, so the convenience is not lost.
	if _, err := r.tl.Lookup("latest"); err != nil {
		t.Fatalf("Lookup(latest): %v", err)
	}
}
