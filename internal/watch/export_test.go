package watch

import (
	"fmt"
	"time"
)

// PruneChangelogForTest exposes the changelog pruning statement to the
// external watch_test package. The retained-set shape it produces is what
// checkResumePointRetained's 410 boundary rests on, so it is worth asserting
// directly rather than only through the once-a-minute cleanup loop.
//
// The `ran` result is dropped: a single-watcher test always wins the advisory
// lock, so a false here would mean the lock leaked from an earlier pass rather
// than a legitimate skip — and the row count would then be a silent zero. It is
// asserted rather than ignored.
func (pw *PostgresWatcher) PruneChangelogForTest(cutoff time.Time) (int64, error) {
	rows, ran, err := pw.pruneChangelog(cutoff)
	if err != nil {
		return rows, err
	}
	if !ran {
		return 0, fmt.Errorf("compaction skipped: the advisory lock was already held, " +
			"which in a single-watcher test means it leaked rather than that another " +
			"replica is compacting")
	}
	return rows, nil
}

// TryPruneChangelogForTest is PruneChangelogForTest without the
// lock-must-be-free assertion, for the test that deliberately contends.
func (pw *PostgresWatcher) TryPruneChangelogForTest(cutoff time.Time) (int64, bool, error) {
	return pw.pruneChangelog(cutoff)
}

// ChangelogCompactionLockIDForTest exposes the advisory lock ID so a test can
// contend for the same lock the replicas do. A test that invented its own ID
// would prove two sessions can contend, not that these two do.
const ChangelogCompactionLockIDForTest = changelogCompactionLockID
