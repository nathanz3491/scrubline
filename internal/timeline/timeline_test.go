package timeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestParseAge(t *testing.T) {
	cases := map[string]time.Duration{
		"7d":  7 * 24 * time.Hour,
		"2w":  14 * 24 * time.Hour,
		"12h": 12 * time.Hour,
		"30m": 30 * time.Minute,
	}
	for in, want := range cases {
		got, err := ParseAge(in)
		if err != nil || got != want {
			t.Fatalf("ParseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "soon", "-3d", "0d", "7x", "d"} {
		if _, err := ParseAge(bad); err == nil {
			t.Fatalf("ParseAge(%q) accepted a bad age", bad)
		}
	}
}

func TestPruneDropsOldSnapshotsAndKeepsTheNewest(t *testing.T) {
	r := newTestRepo(t)

	// Three snapshots, backdated so that two are old.
	r.write("a.txt", "one\n")
	r.snap("oldest")
	r.write("a.txt", "two\n")
	r.snap("middle")
	r.write("a.txt", "three\n")
	newest := r.snap("newest")

	// Rewrite the two older commits to be 30 days old by rebuilding the chain
	// through the same path prune uses.
	snaps, err := r.tl.List(0)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	parent := ""
	for i := len(snaps) - 1; i >= 0; i-- {
		s := snaps[i]
		when := s.When
		if i > 0 {
			when = old
		}
		parent, err = r.tl.Repo.CommitTreeAt(s.Tree, parent, buildMessage(s.Label, s.Branch, s.Files), when)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.tl.Repo.Git("update-ref", "refs/scrubline/timeline", parent); err != nil {
		t.Fatal(err)
	}

	res, err := r.tl.Prune(7 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 2 || res.Kept != 1 {
		t.Fatalf("prune removed %d kept %d, want 2 and 1", res.Removed, res.Kept)
	}
	after, err := r.tl.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("timeline has %d snapshots after prune, want 1", len(after))
	}
	if after[0].Tree != newest.Tree {
		t.Fatal("prune kept the wrong snapshot")
	}
	if after[0].Label != "newest" {
		t.Fatalf("prune lost the label: %q", after[0].Label)
	}
	// The kept snapshot must still restore correctly.
	r.write("a.txt", "mangled\n")
	if _, err := r.tl.Restore(after[0], RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.read("a.txt") != "three\n" {
		t.Fatalf("restore after prune gave %q", r.read("a.txt"))
	}
}

func TestPruneNeverEmptiesTheTimeline(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.snap("only")

	// Everything is older than a zero-length window, but one must survive.
	res, err := r.tl.Prune(time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	after, err := r.tl.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("prune left %d snapshots, want 1 (result %+v)", len(after), res)
	}
}

// --- the symmetric ignore rule -------------------------------------------

func TestRestoreLeavesAPathIgnoredAfterItWasSnapshottedAlone(t *testing.T) {
	r := newTestRepo(t)
	r.write("app.py", "print(1)\n")
	r.git("add", "-A")
	r.git("-c", "user.email=t@t", "-c", "user.name=T", "commit", "-qm", "init")
	good := r.snap("T0")

	// A secret arrives, gets snapshotted, and is only then ignored -- via
	// .git/info/exclude, which is how personal ignores are usually carried.
	r.write(".env.local", "OPENAI_KEY=sk-real-key\n")
	r.snap("")
	appendFile(t, filepath.Join(r.dir, ".git", "info", "exclude"), ".env.local\n")
	r.write("app.py", "GARBAGE\n")

	res, err := r.tl.Restore(good, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.exists(".env.local") {
		t.Fatal("restore deleted a file git ignores now")
	}
	if got := r.read(".env.local"); got != "OPENAI_KEY=sk-real-key\n" {
		t.Fatalf(".env.local was modified: %q", got)
	}
	for _, c := range res.Plan {
		if c.Path == ".env.local" && c.Op == OpDelete {
			t.Fatal("the plan still contained a delete for an ignored path")
		}
	}
	if r.read("app.py") != "print(1)\n" {
		t.Fatal("the non-ignored file was not restored")
	}
}

func TestSnapshotStopsRecordingAPathOnceItIsIgnored(t *testing.T) {
	r := newTestRepo(t)
	r.write("keep.txt", "keep\n")
	r.write(".env", "SECRET=1\n")
	first := r.snap("")

	files, err := r.tl.Files(first)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(files, ".env") {
		t.Fatal("precondition failed: .env should be in the first snapshot")
	}

	// Ignore it after the fact. Every later snapshot must drop it.
	r.write(".gitignore", ".env\n")
	r.write("keep.txt", "changed\n")
	later := r.snap("")

	out, err := r.tl.Repo.GitRaw("ls-tree", "-r", "--name-only", "-z", later.Tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range splitNUL(out) {
		if p == ".env" {
			t.Fatal("a newly ignored path is still being recorded into snapshots")
		}
	}
	if !r.exists(".env") {
		t.Fatal("snapshotting deleted the ignored file from disk")
	}
}

// --- pre-flight -----------------------------------------------------------

func TestRestoreRefusesAFileDirectoryCollisionAndWritesNothing(t *testing.T) {
	r := newTestRepo(t)
	r.write("src/aaa.txt", "aaa\n")
	r.write("src/zzz.txt", "zzz\n")
	good := r.snap("good")

	r.write("src/aaa.txt", "WRECKED\n")
	r.remove("src/zzz.txt")
	if err := os.MkdirAll(filepath.Join(r.dir, "src", "zzz.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.write("src/zzz.txt/x", "junk\n")

	before, err := r.tl.List(0)
	if err != nil {
		t.Fatal(err)
	}

	res, err := r.tl.Restore(good, RestoreOptions{})
	var pf *PreflightError
	if !errors.As(err, &pf) {
		t.Fatalf("expected a pre-flight refusal, got %v", err)
	}
	if res.Partial {
		t.Fatal("a pre-flight refusal must not report a partial write")
	}
	// Nothing written: the other file in the plan is untouched.
	if got := r.read("src/aaa.txt"); got != "WRECKED\n" {
		t.Fatalf("src/aaa.txt was written despite the refusal: %q", got)
	}
	if got := r.read("src/zzz.txt/x"); got != "junk\n" {
		t.Fatal("the colliding directory was disturbed")
	}
	// And no safety snapshot, so retrying cannot pile them up.
	after, err := r.tl.List(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused restore minted %d snapshot(s)", len(after)-len(before))
	}
	if _, err := r.tl.Restore(good, RestoreOptions{}); !errors.As(err, &pf) {
		t.Fatal("the retry did not refuse the same way")
	}
	again, _ := r.tl.List(0)
	if len(again) != len(before) {
		t.Fatalf("retrying minted %d snapshot(s)", len(again)-len(before))
	}
}

// The trap: a directory standing where a file belongs may hold ignored files,
// so it must never be cleared recursively to make room.
func TestCollidingDirectoryHoldingIgnoredFilesIsNeverCleared(t *testing.T) {
	r := newTestRepo(t)
	r.write(".gitignore", "*.log\n")
	r.write("build", "a file called build\n")
	good := r.snap("good")

	// "build" becomes a directory holding only ignored files.
	r.remove("build")
	if err := os.MkdirAll(filepath.Join(r.dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.write("build/output.log", "precious ignored output\n")

	_, err := r.tl.Restore(good, RestoreOptions{})
	if err == nil {
		t.Fatal("expected a refusal rather than clearing the directory")
	}
	if !r.exists("build/output.log") {
		t.Fatal("an ignored file inside a colliding directory was deleted")
	}
	if got := r.read("build/output.log"); got != "precious ignored output\n" {
		t.Fatalf("the ignored file was modified: %q", got)
	}
}

func TestPreflightRefusesAReadOnlyFile(t *testing.T) {
	r := newTestRepo(t)
	r.write("locked.txt", "original\n")
	good := r.snap("")
	r.write("locked.txt", "changed\n")
	if err := os.Chmod(filepath.Join(r.dir, "locked.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(r.dir, "locked.txt"), 0o644) })

	_, err := r.tl.Restore(good, RestoreOptions{})
	var pf *PreflightError
	if !errors.As(err, &pf) {
		t.Fatalf("expected a pre-flight refusal for a read-only file, got %v", err)
	}
	if r.read("locked.txt") != "changed\n" {
		t.Fatal("the read-only file was written anyway")
	}
}

// --- --path that matches nothing -----------------------------------------

func TestUnmatchedPathSelectorIsAnError(t *testing.T) {
	r := newTestRepo(t)
	r.write("src/App.tsx", "good\n")
	good := r.snap("")
	r.write("src/App.tsx", "WRECKED\n")

	for _, dry := range []bool{false, true} {
		_, err := r.tl.Restore(good, RestoreOptions{Path: "src/app.tsx", DryRun: dry})
		if !errors.Is(err, ErrPathNotFound) {
			t.Fatalf("dryRun=%v: expected ErrPathNotFound, got %v", dry, err)
		}
	}
	if r.read("src/App.tsx") != "WRECKED\n" {
		t.Fatal("the tree changed despite the error")
	}

	// A selector that does match is unaffected.
	if _, err := r.tl.Restore(good, RestoreOptions{Path: "src/App.tsx"}); err != nil {
		t.Fatalf("matching selector failed: %v", err)
	}
	if r.read("src/App.tsx") != "good\n" {
		t.Fatal("the matching selector did not restore")
	}
}

func TestPathSelectorThatMatchesButIsUpToDateIsNotAnError(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "one\n")
	r.write("b.txt", "one\n")
	good := r.snap("")
	r.write("b.txt", "two\n") // only b differs

	res, err := r.tl.Restore(good, RestoreOptions{Path: "a.txt"})
	if err != nil {
		t.Fatalf("an up-to-date path should not be an error: %v", err)
	}
	if len(res.Plan) != 0 {
		t.Fatalf("expected an empty plan, got %v", res.Plan)
	}
}

// --- large restores -------------------------------------------------------

func TestRestoreHandlesManyFilesWithoutPathspecArguments(t *testing.T) {
	r := newTestRepo(t)
	const n = 400 // over the pathspec threshold, so the whole tree is listed
	for i := 0; i < n; i++ {
		r.write(fmt.Sprintf("pkg/f%03d.txt", i), fmt.Sprintf("original %d\n", i))
	}
	good := r.snap("")
	for i := 0; i < n; i++ {
		r.write(fmt.Sprintf("pkg/f%03d.txt", i), "WRECKED\n")
	}

	res, err := r.tl.Restore(good, RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan) != n {
		t.Fatalf("plan covered %d files, want %d", len(res.Plan), n)
	}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("pkg/f%03d.txt", i)
		if got, want := r.read(p), fmt.Sprintf("original %d\n", i); got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestPartialRestoreReportsTheSafetySnapshot covers the failure that pre-flight
// cannot predict: a file whose permission bits say writable but which the
// filesystem refuses to touch. macOS's immutable flag gives that deterministically.
func TestPartialRestoreReportsTheSafetySnapshot(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("needs chflags to make a write fail without tripping pre-flight")
	}
	r := newTestRepo(t)
	r.write("aaa.txt", "original\n")
	r.write("zzz.txt", "original\n")
	good := r.snap("good")

	r.write("aaa.txt", "WRECKED\n")
	r.write("zzz.txt", "WRECKED\n")
	wrecked := r.state()

	locked := filepath.Join(r.dir, "zzz.txt")
	if out, err := exec.Command("chflags", "uchg", locked).CombinedOutput(); err != nil {
		t.Skipf("chflags unavailable: %v %s", err, out)
	}
	unlock := func() { exec.Command("chflags", "nouchg", locked).Run() }
	t.Cleanup(unlock)

	res, err := r.tl.Restore(good, RestoreOptions{})
	if err == nil {
		t.Fatal("expected the restore to fail on the immutable file")
	}
	if !res.Partial {
		t.Fatal("a failure after writing began must be reported as partial")
	}
	if res.Safety.ID == "" {
		t.Fatal("a partial restore must report the safety snapshot to recover from")
	}
	// The plan is path-sorted, so the first file was written and the second was not.
	if r.read("aaa.txt") != "original\n" {
		t.Fatal("precondition: expected the first file to have been restored")
	}
	if r.read("zzz.txt") != "WRECKED\n" {
		t.Fatal("precondition: expected the immutable file to be untouched")
	}

	// The reported safety snapshot really does get the user back.
	unlock()
	if _, err := r.tl.Restore(res.Safety, RestoreOptions{}); err != nil {
		t.Fatalf("restoring the reported safety snapshot failed: %v", err)
	}
	if got := r.state(); !sameState(got, wrecked) {
		t.Fatal("the safety snapshot did not restore the pre-restore state")
	}
}
