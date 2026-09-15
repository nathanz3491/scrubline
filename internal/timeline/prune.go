package timeline

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nathanz3491/scrubline/internal/meta"
)

// PruneResult reports what a prune did.
type PruneResult struct {
	Removed int
	Kept    int
}

// Prune drops snapshots older than the given age.
//
// The timeline is a commit chain, so dropping the tail means rebuilding the
// kept snapshots onto a new root. Their ids therefore change; their trees,
// labels, and timestamps do not. The newest snapshot is always kept, so a prune
// can never leave you with nothing to go back to.
//
// Pruning only unlinks the old commits. The objects stay in the repository
// until git garbage-collects them, which scrubline deliberately does not do on
// the user's behalf -- see PruneHint.
func (t *Timeline) Prune(olderThan time.Duration) (PruneResult, error) {
	unlock, err := t.lock()
	if err != nil {
		return PruneResult{}, err
	}
	defer unlock()

	snaps, err := t.List(0) // newest first
	if err != nil {
		return PruneResult{}, err
	}
	if len(snaps) == 0 {
		return PruneResult{}, nil
	}

	cutoff := time.Now().Add(-olderThan)
	var keep []Snapshot
	for _, s := range snaps {
		if s.When.After(cutoff) {
			keep = append(keep, s)
		}
	}
	if len(keep) == 0 {
		// Never leave the timeline empty.
		keep = []Snapshot{snaps[0]}
	}
	if len(keep) == len(snaps) {
		return PruneResult{Kept: len(snaps)}, nil
	}

	// Rebuild oldest-first onto a fresh root.
	parent := ""
	for i := len(keep) - 1; i >= 0; i-- {
		s := keep[i]
		id, err := t.Repo.CommitTreeAt(s.Tree, parent, buildMessage(s.Label, s.Branch, s.Files), s.When)
		if err != nil {
			return PruneResult{}, fmt.Errorf("rebuilding the timeline: %w", err)
		}
		parent = id
	}
	if _, err := t.Repo.Git("update-ref", meta.TimelineRef, parent); err != nil {
		return PruneResult{}, err
	}
	return PruneResult{Removed: len(snaps) - len(keep), Kept: len(keep)}, nil
}

// PruneHint is the command that actually reclaims the disk space, which is left
// to the user: `git gc` prunes every unreferenced object in the repository, not
// only scrubline's, and running that on someone's behalf is not this tool's
// call to make.
const PruneHint = "git gc --prune=now"

// ParseAge parses a duration that may use a day or week suffix, which
// time.ParseDuration does not accept.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty age")
	}
	unit := s[len(s)-1]
	var mult time.Duration
	switch unit {
	case 'd':
		mult = 24 * time.Hour
	case 'w':
		mult = 7 * 24 * time.Hour
	default:
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%q is not an age like 30m, 12h, 7d, or 2w", s)
		}
		if d <= 0 {
			return 0, fmt.Errorf("age must be positive")
		}
		return d, nil
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not an age like 30m, 12h, 7d, or 2w", s)
	}
	return time.Duration(n * float64(mult)), nil
}
