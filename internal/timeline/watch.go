package timeline

import (
	"context"
	"time"
)

// WatchOptions tunes the recorder.
type WatchOptions struct {
	// Interval is how often the working tree is examined.
	Interval time.Duration
	// Debounce is how long the tree must stop changing before a snapshot is
	// taken, so that a burst of edits becomes one snapshot rather than many.
	Debounce time.Duration
}

// WatchEvent reports something the recorder did, for display.
type WatchEvent struct {
	// Snapshot is set when a snapshot was created.
	Snapshot *Snapshot
	// Err is set when a poll failed; the recorder keeps running.
	Err error
	// Polled is set on every examination that produced neither of the above.
	Polled bool
	// Dirty reports whether unsnapshotted changes are currently pending.
	Dirty bool
}

// Watch records snapshots until ctx is cancelled.
//
// Change detection compares the staged tree id rather than `git status` output,
// so an edit to an already-modified file is still detected. The cost of doing
// that every interval is held down by the persistent index: git only re-hashes
// files whose stat information changed, which is the same work `git status`
// does, so an idle repository costs almost nothing.
func (t *Timeline) Watch(ctx context.Context, opts WatchOptions, emit func(WatchEvent)) error {
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 1500 * time.Millisecond
	}

	known := ""
	if tip, ok, err := t.Tip(); err == nil && ok {
		known = tip.Tree
	}
	// Capture whatever is already uncommitted when the recorder starts, so the
	// state the user began with is never the one state that was not recorded.
	if snap, created, err := t.Snap(""); err == nil {
		known = snap.Tree
		if created {
			s := snap
			emit(WatchEvent{Snapshot: &s})
		}
	}

	var dirtySince time.Time
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		tree, err := t.StageTree()
		if err != nil {
			emit(WatchEvent{Err: err})
			continue
		}
		if tree != known {
			known = tree
			dirtySince = time.Now()
			emit(WatchEvent{Polled: true, Dirty: true})
			continue
		}
		if dirtySince.IsZero() || time.Since(dirtySince) < opts.Debounce {
			emit(WatchEvent{Polled: true, Dirty: !dirtySince.IsZero()})
			continue
		}

		dirtySince = time.Time{}
		snap, created, err := t.Snap("")
		if err != nil {
			emit(WatchEvent{Err: err})
			continue
		}
		known = snap.Tree
		if created {
			s := snap
			emit(WatchEvent{Snapshot: &s})
		} else {
			emit(WatchEvent{Polled: true})
		}
	}
}
