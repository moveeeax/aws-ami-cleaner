package cleaner

import (
	"context"
	"errors"
	"testing"
	"time"
)

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

var ref = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// ami is a small constructor for readable test fixtures.
func ami(id, name string, ageDays int, gib int64, tags map[string]string) AMI {
	return AMI{
		ID:        id,
		Name:      name,
		Created:   ref.Add(-day(ageDays)),
		Tags:      tags,
		Snapshots: []Snapshot{{ID: "snap-" + id, SizeGiB: gib}},
	}
}

func ids(ds []Deletion) map[string]bool {
	m := map[string]bool{}
	for _, d := range ds {
		m[d.AMI.ID] = true
	}
	return m
}

func TestParseAge(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"90d", 90 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1.5d", 36 * time.Hour, false},
		{"", 0, false},
		{"90", 0, true},   // bare number is not a valid stdlib duration
		{"7y", 0, true},   // unsupported unit
		{"abcd", 0, true}, // no number
	}
	for _, c := range cases {
		got, err := ParseAge(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseAge(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAge(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAge(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPlanAgeAndKeepLast(t *testing.T) {
	// One lineage "web", 5 images from newest (10d) to oldest (200d).
	amis := []AMI{
		ami("ami-1", "web", 10, 8, nil),
		ami("ami-2", "web", 40, 8, nil),
		ami("ami-3", "web", 100, 8, nil),
		ami("ami-4", "web", 150, 8, nil),
		ami("ami-5", "web", 200, 8, nil),
	}
	p := Policy{KeepLast: 3, OlderThan: day(90)}
	rep := Plan(amis, nil, p, ref, DefaultSnapshotPriceUSD)

	del := ids(rep.Delete)
	// Keep newest 3 (ami-1,2,3). Of the rest, only those older than 90d go:
	// ami-4 (150d) and ami-5 (200d).
	if len(rep.Delete) != 2 || !del["ami-4"] || !del["ami-5"] {
		t.Fatalf("unexpected deletions: %v", del)
	}
	if rep.TotalGiB != 16 {
		t.Errorf("TotalGiB = %d, want 16", rep.TotalGiB)
	}
	wantSav := 16 * DefaultSnapshotPriceUSD
	if rep.TotalSavingsUSD != wantSav {
		t.Errorf("savings = %v, want %v", rep.TotalSavingsUSD, wantSav)
	}
}

func TestPlanKeepLastProtectsOldImage(t *testing.T) {
	// Only 2 images, both old. keep-last 3 must protect both even past the age.
	amis := []AMI{
		ami("ami-1", "db", 300, 4, nil),
		ami("ami-2", "db", 400, 4, nil),
	}
	rep := Plan(amis, nil, Policy{KeepLast: 3, OlderThan: day(90)}, ref, 0.05)
	if len(rep.Delete) != 0 {
		t.Fatalf("expected no deletions, got %v", ids(rep.Delete))
	}
}

func TestPlanNeverTouchesInUse(t *testing.T) {
	amis := []AMI{
		ami("ami-old", "web", 365, 20, nil),
	}
	inUse := map[string]struct{}{"ami-old": {}}
	rep := Plan(amis, inUse, Policy{OlderThan: day(90)}, ref, 0.05)
	if len(rep.Delete) != 0 {
		t.Fatalf("in-use AMI was selected: %v", ids(rep.Delete))
	}
	if len(rep.Keep) != 1 || rep.Keep[0].Reason == "" {
		t.Fatalf("expected a kept reason, got %+v", rep.Keep)
	}
}

func TestPlanTagFilter(t *testing.T) {
	amis := []AMI{
		ami("ami-a", "web", 200, 5, map[string]string{"env": "dev"}),
		ami("ami-b", "web", 200, 5, map[string]string{"env": "prod"}),
		ami("ami-c", "web", 200, 5, nil),
	}
	p := Policy{OlderThan: day(90), TagFilter: map[string]string{"env": "dev"}}
	rep := Plan(amis, nil, p, ref, 0.05)
	del := ids(rep.Delete)
	if len(del) != 1 || !del["ami-a"] {
		t.Fatalf("tag filter mis-selected: %v", del)
	}
}

func TestPlanUnnamedGroupsIndependently(t *testing.T) {
	// Two un-named images: keep-last must not let one protect the other.
	amis := []AMI{
		{ID: "ami-x", Created: ref.Add(-day(200)), Snapshots: []Snapshot{{ID: "s1", SizeGiB: 1}}},
		{ID: "ami-y", Created: ref.Add(-day(300)), Snapshots: []Snapshot{{ID: "s2", SizeGiB: 1}}},
	}
	rep := Plan(amis, nil, Policy{KeepLast: 1, OlderThan: day(90)}, ref, 0.05)
	// KeepLast groups per-name; unnamed images are singletons so keep-last=1
	// protects each of them — nothing should be deleted.
	if len(rep.Delete) != 0 {
		t.Fatalf("unnamed images grouped together: %v", ids(rep.Delete))
	}
}

// fakeDeleter records calls and can fail on a chosen ID.
type fakeDeleter struct {
	deregistered []string
	deleted      []string
	failOn       string
}

func (f *fakeDeleter) DeregisterImage(_ context.Context, id string) error {
	if id == f.failOn {
		return errors.New("boom")
	}
	f.deregistered = append(f.deregistered, id)
	return nil
}

func (f *fakeDeleter) DeleteSnapshot(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestApplyExecutesInOrder(t *testing.T) {
	amis := []AMI{ami("ami-1", "web", 200, 8, nil), ami("ami-2", "web", 210, 8, nil)}
	rep := Plan(amis, nil, Policy{OlderThan: day(90)}, ref, 0.05)
	f := &fakeDeleter{}
	if err := Apply(context.Background(), f, rep, false); err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if len(f.deregistered) != 2 || len(f.deleted) != 2 {
		t.Fatalf("expected 2 deregisters + 2 snapshot deletes, got %v / %v", f.deregistered, f.deleted)
	}
}

func TestApplyDryRunDoesNothing(t *testing.T) {
	amis := []AMI{ami("ami-1", "web", 200, 8, nil)}
	rep := Plan(amis, nil, Policy{OlderThan: day(90)}, ref, 0.05)
	f := &fakeDeleter{}
	if err := Apply(context.Background(), f, rep, true); err != nil {
		t.Fatalf("Apply dry-run error: %v", err)
	}
	if len(f.deregistered)+len(f.deleted) != 0 {
		t.Fatalf("dry-run mutated: %v %v", f.deregistered, f.deleted)
	}
}

func TestApplyStopsOnDeregisterError(t *testing.T) {
	amis := []AMI{ami("ami-1", "web", 200, 8, nil)}
	rep := Plan(amis, nil, Policy{OlderThan: day(90)}, ref, 0.05)
	f := &fakeDeleter{failOn: "ami-1"}
	if err := Apply(context.Background(), f, rep, false); err == nil {
		t.Fatal("expected error, got nil")
	}
	if len(f.deleted) != 0 {
		t.Fatalf("snapshot deleted despite failed deregister: %v", f.deleted)
	}
}
