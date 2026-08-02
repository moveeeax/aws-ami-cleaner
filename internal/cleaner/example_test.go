package cleaner_test

import (
	"fmt"
	"time"

	"github.com/moveeeax/aws-ami-cleaner/internal/cleaner"
)

// ExamplePlan shows the engine deciding a plan for a single "web" lineage under
// a "keep the last 2, delete anything older than 90 days" policy, with one image
// pinned in-use. It doubles as a golden test of the summary output.
func ExamplePlan() {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, name string, ageDays int, gib int64) cleaner.AMI {
		return cleaner.AMI{
			ID:        id,
			Name:      name,
			Created:   now.Add(-time.Duration(ageDays) * 24 * time.Hour),
			Snapshots: []cleaner.Snapshot{{ID: "snap-" + id, SizeGiB: gib}},
		}
	}
	amis := []cleaner.AMI{
		mk("ami-0001", "web", 5, 8),   // newest — kept (keep-last)
		mk("ami-0002", "web", 30, 8),  // kept (keep-last)
		mk("ami-0003", "web", 120, 8), // old, but pinned in-use
		mk("ami-0004", "web", 200, 8), // deleted
	}
	inUse := map[string]struct{}{"ami-0003": {}}
	policy := cleaner.Policy{KeepLast: 2, OlderThan: 90 * 24 * time.Hour}

	rep := cleaner.Plan(amis, inUse, policy, now, cleaner.DefaultSnapshotPriceUSD)
	fmt.Print(rep.Summary("us-east-1", true))
	// Output:
	// [DRY-RUN] region=us-east-1: 1 to delete, 3 kept
	//   DELETE ami-0004 web                          2026-01-13  8GiB  ~$0.40/mo
	//   reclaim: 8GiB  ~$0.40/mo (@ $0.050/GiB-mo)
}
