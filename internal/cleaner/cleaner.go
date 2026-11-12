// Package cleaner holds the pure retention engine for aws-ami-cleaner.
//
// Everything in this file is deliberately free of the AWS SDK: it operates on
// plain domain types so the selection logic can be unit-tested exhaustively
// without touching a real account. The SDK adapter lives in internal/awssrc and
// only feeds these types in and executes the decisions this package returns.
package cleaner

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot is an EBS snapshot backing an AMI's block device mapping.
type Snapshot struct {
	ID      string
	SizeGiB int64
}

// AMI is a self-owned Amazon Machine Image plus the snapshots it would orphan.
type AMI struct {
	ID      string
	Name    string
	Created time.Time
	// LastLaunched is when EC2 last reported an instance launch from this image.
	// The zero value means it was never launched (or before April 2017, when EC2
	// started recording it).
	LastLaunched time.Time
	// Shared is true when the image has launch permissions for another account,
	// an organization or OU, or the public. Other accounts may depend on it even
	// though nothing in this account references it.
	Shared    bool
	Tags      map[string]string
	Snapshots []Snapshot
}

// lastUsed is the reference point for UnusedFor: the last launch, or the
// creation time for an image that was never launched.
func (a AMI) lastUsed() time.Time {
	if a.LastLaunched.IsZero() {
		return a.Created
	}
	return a.LastLaunched
}

// sizeGiB totals the snapshot storage an AMI would release when deleted.
func (a AMI) sizeGiB() int64 {
	var n int64
	for _, s := range a.Snapshots {
		n += s.SizeGiB
	}
	return n
}

// Policy is the retention rule set. A zeroed field disables that dimension, and
// every enabled dimension must agree that an image is expendable before it is
// selected — the engine is protection-first by construction.
type Policy struct {
	KeepLast  int               // keep the newest N images per Name group (0 = off)
	OlderThan time.Duration     // only images at least this old are candidates (0 = off)
	TagFilter map[string]string // only images carrying all of these tags are candidates
	// UnusedFor makes an image a candidate only when it has not been launched for
	// at least this long; a never-launched image counts from its creation (0 = off).
	UnusedFor time.Duration
	// IgnoreSharing lets shared images be deleted. By default they are kept.
	IgnoreSharing bool
}

// Deletion is a single AMI the engine decided to remove, with its freed storage.
type Deletion struct {
	AMI     AMI
	SizeGiB int64
	// SavingsUSD is the estimated recurring monthly snapshot cost reclaimed.
	SavingsUSD float64
}

// Kept is an image the engine spared, with a short human reason.
type Kept struct {
	AMI    AMI
	Reason string
}

// Report is the full plan: what would be deleted, what was kept and why, and the
// rolled-up savings. It is produced by Plan and consumed by Apply.
type Report struct {
	Delete           []Deletion
	Keep             []Kept
	TotalGiB         int64
	TotalSavingsUSD  float64
	PricePerGiBMonth float64
}

// DefaultSnapshotPriceUSD is the us-east-1 EBS snapshot rate ($/GiB-month). It is
// the standard published figure and is overridable via the CLI.
const DefaultSnapshotPriceUSD = 0.05

// ParseAge parses a compact duration like "90d", "2w", "12h", "30m" or "45s".
// Go's time.ParseDuration rejects days and weeks, which are the units that
// actually matter for AMI retention, so this fills the gap.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	// Split trailing unit letters from the leading number.
	i := 0
	for i < len(s) && (s[i] == '-' || s[i] == '+' || s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, unit := s[:i], s[i:]
	if num == "" {
		return 0, fmt.Errorf("invalid age %q: missing number", s)
	}
	switch unit {
	case "d", "w":
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q: %w", s, err)
		}
		mult := 24.0
		if unit == "w" {
			mult = 24.0 * 7.0
		}
		return time.Duration(v * mult * float64(time.Hour)), nil
	case "", "h", "m", "s", "ms", "us", "ns":
		// Defer to the stdlib; a bare number is invalid there, which is correct.
		return time.ParseDuration(s)
	default:
		return 0, fmt.Errorf("invalid age %q: unknown unit %q", s, unit)
	}
}

// matchesTags reports whether an AMI carries every key/value in the filter.
func matchesTags(a AMI, filter map[string]string) bool {
	for k, want := range filter {
		if got, ok := a.Tags[k]; !ok || got != want {
			return false
		}
	}
	return true
}

// Plan applies the policy to a set of owned AMIs and returns the deletion plan.
//
// inUse is the set of AMI IDs that are referenced by running instances, launch
// templates or Auto Scaling groups; those are never selected regardless of age.
// price is the $/GiB-month used to estimate savings.
func Plan(amis []AMI, inUse map[string]struct{}, p Policy, now time.Time, price float64) *Report {
	rep := &Report{PricePerGiBMonth: price}

	// Group by Name so KeepLast counts per image lineage. Images without a Name
	// each form their own singleton group, which means KeepLast alone never
	// deletes an un-named image — age or tags must also opt it in.
	groups := map[string][]AMI{}
	var order []string
	for _, a := range amis {
		key := a.Name
		if key == "" {
			key = "\x00id:" + a.ID // unique per image
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], a)
	}

	for _, key := range order {
		g := groups[key]
		// Newest first so rank 0..KeepLast-1 are the ones we retain.
		sort.SliceStable(g, func(i, j int) bool {
			if !g[i].Created.Equal(g[j].Created) {
				return g[i].Created.After(g[j].Created)
			}
			return g[i].ID < g[j].ID
		})

		for rank, a := range g {
			if _, ok := inUse[a.ID]; ok {
				rep.keep(a, "in use (instance/LT/ASG)")
				continue
			}
			if a.Shared && !p.IgnoreSharing {
				rep.keep(a, "shared")
				continue
			}
			if len(p.TagFilter) > 0 && !matchesTags(a, p.TagFilter) {
				rep.keep(a, "does not match tag filter")
				continue
			}
			if p.KeepLast > 0 && rank < p.KeepLast {
				rep.keep(a, fmt.Sprintf("within keep-last %d", p.KeepLast))
				continue
			}
			if p.OlderThan > 0 && now.Sub(a.Created) < p.OlderThan {
				rep.keep(a, "younger than retention age")
				continue
			}
			if p.UnusedFor > 0 && now.Sub(a.lastUsed()) < p.UnusedFor {
				rep.keep(a, "launched recently")
				continue
			}
			rep.del(a, price)
		}
	}
	return rep
}

func (r *Report) keep(a AMI, reason string) {
	r.Keep = append(r.Keep, Kept{AMI: a, Reason: reason})
}

func (r *Report) del(a AMI, price float64) {
	sz := a.sizeGiB()
	sav := float64(sz) * price
	r.Delete = append(r.Delete, Deletion{AMI: a, SizeGiB: sz, SavingsUSD: sav})
	r.TotalGiB += sz
	r.TotalSavingsUSD += sav
}

// Deleter performs the two mutating calls the engine needs. The AWS adapter and
// the test fakes both satisfy it.
type Deleter interface {
	DeregisterImage(ctx context.Context, amiID string) error
	DeleteSnapshot(ctx context.Context, snapshotID string) error
}

// Apply executes a report: for each planned deletion it deregisters the AMI and
// then deletes each backing snapshot. When dryRun is true it performs no calls.
// Snapshots are only touched after the image is successfully deregistered, so a
// mid-run failure can never orphan a snapshot from a still-registered AMI.
func Apply(ctx context.Context, d Deleter, rep *Report, dryRun bool) error {
	if dryRun {
		return nil
	}
	for _, del := range rep.Delete {
		if err := d.DeregisterImage(ctx, del.AMI.ID); err != nil {
			return fmt.Errorf("deregister %s: %w", del.AMI.ID, err)
		}
		for _, s := range del.AMI.Snapshots {
			if err := d.DeleteSnapshot(ctx, s.ID); err != nil {
				return fmt.Errorf("delete snapshot %s (ami %s): %w", s.ID, del.AMI.ID, err)
			}
		}
	}
	return nil
}

// Summary renders a stable, human-readable plan. Amounts use two decimals so the
// dry-run output is diff-friendly across runs.
func (r *Report) Summary(region string, dryRun bool) string {
	var b strings.Builder
	mode := "APPLY"
	if dryRun {
		mode = "DRY-RUN"
	}
	fmt.Fprintf(&b, "[%s] region=%s: %d to delete, %d kept\n", mode, region, len(r.Delete), len(r.Keep))
	if len(r.Delete) > 0 {
		fmt.Fprintf(&b, "         %-*s %-28s %-10s  %-11s\n", idWidth(r.Delete), "ID", "NAME", "CREATED", "LAST LAUNCH")
	}
	for _, d := range r.Delete {
		name := d.AMI.Name
		if name == "" {
			name = "(no name)"
		}
		launched := "never"
		if !d.AMI.LastLaunched.IsZero() {
			launched = d.AMI.LastLaunched.UTC().Format("2006-01-02")
		}
		fmt.Fprintf(&b, "  DELETE %-*s %-28s %s  %-11s  %dGiB  ~$%.2f/mo\n",
			idWidth(r.Delete), d.AMI.ID, truncate(name, 28), d.AMI.Created.UTC().Format("2006-01-02"), launched, d.SizeGiB, d.SavingsUSD)
	}
	if line := keptBreakdown(r.Keep); line != "" {
		fmt.Fprintf(&b, "  kept: %s\n", line)
	}
	fmt.Fprintf(&b, "  reclaim: %dGiB  ~$%.2f/mo (@ $%.3f/GiB-mo)\n", r.TotalGiB, r.TotalSavingsUSD, r.PricePerGiBMonth)
	return b.String()
}

// idWidth is the widest AMI ID in the plan, so the ID column lines up with its
// header whether the account uses 8- or 17-character IDs.
func idWidth(ds []Deletion) int {
	w := len("ID")
	for _, d := range ds {
		if len(d.AMI.ID) > w {
			w = len(d.AMI.ID)
		}
	}
	return w
}

// keptBreakdown counts kept images per reason, e.g. "2 in use (instance/LT/ASG),
// 1 shared". Reasons are sorted so the line is stable across runs.
func keptBreakdown(ks []Kept) string {
	if len(ks) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, k := range ks {
		counts[k.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%d %s", counts[r], r))
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
