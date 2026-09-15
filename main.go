// Command scrubline records your working tree while an agent edits it, and puts
// it back the way it was.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nathanz3491/scrubline/internal/gitx"
	"github.com/nathanz3491/scrubline/internal/meta"
	"github.com/nathanz3491/scrubline/internal/timeline"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", meta.Name, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}
	switch args[0] {
	case "snap":
		return cmdSnap(args[1:])
	case "list", "ls":
		return cmdList(args[1:])
	case "show":
		return cmdShow(args[1:])
	case "watch":
		return cmdWatch(args[1:])
	case "mark":
		return cmdMark(args[1:])
	case "restore":
		return cmdRestore(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("%s %s\n", meta.Name, meta.Version)
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, usageText, meta.Name, meta.Version, meta.TimelineRef, meta.RepoURL)
}

const usageText = "%[1]s %[2]s -- an undo timeline for agent-edited working trees.\n" +
	"\n" +
	"Usage:\n" +
	"  %[1]s watch                 record snapshots continuously while you work\n" +
	"  %[1]s snap [-m LABEL]       take one snapshot now\n" +
	"  %[1]s mark LABEL            label the next snapshot (for agent hooks)\n" +
	"  %[1]s list [-n N]           list snapshots, newest first\n" +
	"  %[1]s show ID [--files]     show what a snapshot changed\n" +
	"  %[1]s restore ID [flags]    put the working tree back to a snapshot\n" +
	"  %[1]s version\n" +
	"\n" +
	"Snapshots are git commits on %[3]s, so they cost almost nothing\n" +
	"and never touch your index, HEAD, or branches. Files ignored by git are never\n" +
	"recorded, and so are never restored or deleted.\n" +
	"\n" +
	"Run `%[1]s COMMAND -h` for the flags of one command.\n" +
	"%[4]s\n"

// open resolves the timeline for the current directory, turning "you are not in
// a git repository" into a one-line actionable error rather than a stack trace.
func open() (*timeline.Timeline, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	t, err := timeline.Open(dir)
	if errors.Is(err, gitx.ErrNotARepo) {
		return nil, fmt.Errorf("%s needs a git repository, and %s is not inside one (run `git init` first)", meta.Name, dir)
	}
	return t, err
}

func newFlagSet(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(meta.Name+" "+name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s %s\n\n%s\n", meta.Name, name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs parses flags that appear before, after, or between positional
// arguments. The standard flag package stops at the first positional, which
// would silently ignore the flag in `scrubline restore ID --dry-run` -- exactly
// the form the documentation uses.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func cmdSnap(args []string) error {
	fs := newFlagSet("snap [-m LABEL]", "Take one snapshot of the working tree now.\nDoes nothing if nothing changed since the last snapshot.")
	label := fs.String("m", "", "label for this snapshot")
	if _, err := parseArgs(fs, args); err != nil {
		return errParsed(err)
	}
	t, err := open()
	if err != nil {
		return err
	}
	snap, created, err := t.Snap(*label)
	if err != nil {
		return err
	}
	if !created {
		fmt.Printf("nothing changed since %s, no new snapshot\n", snap.ShortID())
		return nil
	}
	fmt.Printf("%s  %s\n", snap.ShortID(), describeSnap(snap))
	return nil
}

func cmdList(args []string) error {
	fs := newFlagSet("list [-n N]", "List snapshots, newest first.")
	limit := fs.Int("n", 20, "maximum number of snapshots to show, 0 for all")
	if _, err := parseArgs(fs, args); err != nil {
		return errParsed(err)
	}
	t, err := open()
	if err != nil {
		return err
	}
	snaps, err := t.List(*limit)
	if err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Printf("no snapshots yet -- run `%s watch` and start working\n", meta.Name)
		return nil
	}
	for _, s := range snaps {
		fmt.Printf("%s  %s\n", s.ShortID(), describeSnap(s))
	}
	return nil
}

func cmdShow(args []string) error {
	fs := newFlagSet("show ID [--files]", "Show what a snapshot changed.")
	files := fs.Bool("files", false, "list the files the snapshot changed, one per line")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return errParsed(err)
	}
	t, err := open()
	if err != nil {
		return err
	}
	id := first(rest)
	if strings.TrimSpace(id) == "" {
		id = "latest"
	}
	snap, err := t.Lookup(id)
	if err != nil {
		return err
	}
	if *files {
		names, err := t.Files(snap)
		if err != nil {
			return err
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	}
	fmt.Printf("%s  %s\n", snap.ShortID(), describeSnap(snap))
	names, err := t.Files(snap)
	if err != nil {
		return err
	}
	for _, n := range names {
		fmt.Printf("  %s\n", n)
	}
	return nil
}

func cmdMark(args []string) error {
	fs := newFlagSet("mark LABEL", "Label the next snapshot.\n\nIf the working tree has changed, a labelled snapshot is taken immediately;\notherwise the label waits for the next snapshot. This is the seam for agent\nhooks: call it at a turn boundary and the timeline reads as turns.")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return errParsed(err)
	}
	label := strings.TrimSpace(strings.Join(rest, " "))
	if label == "" {
		return errors.New("mark needs a label, e.g. " + meta.Name + " mark \"before refactor\"")
	}
	t, err := open()
	if err != nil {
		return err
	}
	if err := t.SetPendingLabel(label); err != nil {
		return err
	}
	snap, created, err := t.Snap("")
	if err != nil {
		return err
	}
	if !created {
		fmt.Printf("%q will label the next snapshot\n", label)
		return nil
	}
	fmt.Printf("%s  %s\n", snap.ShortID(), describeSnap(snap))
	return nil
}

func cmdWatch(args []string) error {
	fs := newFlagSet("watch [flags]", "Record snapshots continuously until interrupted.")
	interval := fs.Duration("interval", 2*time.Second, "how often to examine the working tree")
	debounce := fs.Duration("debounce", 1500*time.Millisecond, "how long edits must pause before a snapshot is taken")
	quiet := fs.Bool("quiet", false, "only print snapshots, never the status line")
	if _, err := parseArgs(fs, args); err != nil {
		return errParsed(err)
	}
	t, err := open()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tty := isTTY(os.Stdout) && !*quiet
	count := 0
	started := time.Now()

	fmt.Printf("recording %s -- press Ctrl-C to stop\n", t.Repo.Root)
	err = t.Watch(ctx, timeline.WatchOptions{Interval: *interval, Debounce: *debounce}, func(ev timeline.WatchEvent) {
		switch {
		case ev.Err != nil:
			clearStatus(tty)
			fmt.Fprintf(os.Stderr, "%s: %v\n", meta.Name, ev.Err)
		case ev.Snapshot != nil:
			count++
			clearStatus(tty)
			fmt.Printf("%s  %s\n", ev.Snapshot.ShortID(), describeSnap(*ev.Snapshot))
		case tty:
			state := "idle"
			if ev.Dirty {
				state = "changes pending"
			}
			fmt.Printf("\r\033[K  %s | %d snapshot%s | %s", state, count, plural(count), shortDuration(time.Since(started)))
		}
	})
	clearStatus(tty)
	if err != nil {
		return err
	}
	fmt.Printf("stopped after %s, %d snapshot%s -- `%s list` to see them\n",
		shortDuration(time.Since(started)), count, plural(count), meta.Name)
	return nil
}

func cmdRestore(args []string) error {
	fs := newFlagSet("restore ID [flags]", "Put the working tree back to a snapshot.\n\nThe current state is always snapshotted first, so a restore can itself be\nundone. Files ignored by git are never written or deleted.")
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	path := fs.String("path", "", "restore only this file or directory")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return errParsed(err)
	}
	// Unlike show, restore never guesses: rewriting the working tree is not
	// something to do because an argument was forgotten. A blank argument counts
	// as forgotten -- `restore "$SNAP"` with an unset SNAP must not resolve to
	// the newest snapshot.
	if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
		return fmt.Errorf("restore needs a snapshot id (`%s list` shows them, `latest` names the newest)", meta.Name)
	}
	t, err := open()
	if err != nil {
		return err
	}
	snap, err := t.Lookup(rest[0])
	if err != nil {
		return err
	}
	res, err := t.Restore(snap, timeline.RestoreOptions{DryRun: *dryRun, Path: *path})
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Printf("dry run, restoring %s would:\n%s\n", snap.ShortID(), timeline.Describe(res.Plan))
		return nil
	}
	if len(res.Plan) == 0 {
		fmt.Printf("the working tree already matches %s, nothing to do\n", snap.ShortID())
		return nil
	}
	fmt.Printf("restored %s\n%s\n", snap.ShortID(), timeline.Describe(res.Plan))
	fmt.Printf("\nthe previous state is snapshot %s -- `%s restore %[1]s` puts it back\n",
		res.Safety.ShortID(), meta.Name)
	return nil
}

func describeSnap(s timeline.Snapshot) string {
	parts := []string{fmt.Sprintf("%-8s", relativeTime(s.When))}
	if s.Branch != "" {
		parts = append(parts, fmt.Sprintf("%-12s", s.Branch))
	}
	parts = append(parts, fmt.Sprintf("%d file%s", s.Files, plural(s.Files)))
	line := strings.Join(parts, "  ")
	if s.Label != "" {
		line += "  " + s.Label
	}
	return line
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
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

func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func clearStatus(tty bool) {
	if tty {
		fmt.Print("\r\033[K")
	}
}

func isTTY(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// errParsed swallows flag.ErrHelp so that `-h` exits zero without printing an
// error line under the help text.
func errParsed(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}
