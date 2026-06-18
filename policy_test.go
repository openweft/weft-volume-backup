package weftvolumebackup

import (
	"testing"
	"time"
)

func snap(day, hour int) Snapshot {
	t := time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC)
	return Snapshot{Tag: t.Format(time.RFC3339), CreatedAt: t}
}

func TestRetainGFS(t *testing.T) {
	// 3 snapshots on day 10, then one per day 9..1 (hourly + daily history).
	var snaps []Snapshot
	snaps = append(snaps, snap(10, 8), snap(10, 12), snap(10, 18))
	for d := 9; d >= 1; d-- {
		snaps = append(snaps, snap(d, 12))
	}
	now := time.Date(2026, 1, 10, 23, 0, 0, 0, time.UTC)

	// keep last 2 + 4 daily.
	p := Policy{KeepLast: 2, KeepDaily: 4}
	keep, expire := p.Retain(snaps, now)

	// Expect: the 2 newest of day 10 (last) + the daily survivors of days 10,9,8,7
	// (day-10 survivor overlaps 'last'). Net keep = {10@18,10@12 (last), 9@12,8@12,7@12 (daily)} = 5.
	if len(keep) != 5 {
		t.Fatalf("keep=%d want 5: %v", len(keep), tags(keep))
	}
	if len(keep)+len(expire) != len(snaps) {
		t.Fatalf("keep+expire=%d != %d", len(keep)+len(expire), len(snaps))
	}
	// day 10 @ 08:00 must be expired (not in last-2, day-10 daily already taken by 18:00).
	for _, s := range keep {
		if s.CreatedAt.Equal(snap(10, 8).CreatedAt) {
			t.Fatal("10@08 should have been expired")
		}
	}
}

func TestDueAt(t *testing.T) {
	p := Policy{Interval: time.Hour}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if !p.DueAt(time.Time{}, now) {
		t.Fatal("never-snapshotted should be due")
	}
	if p.DueAt(now.Add(-30*time.Minute), now) {
		t.Fatal("30min < 1h interval: not due")
	}
	if !p.DueAt(now.Add(-2*time.Hour), now) {
		t.Fatal("2h >= 1h: due")
	}
	if (Policy{}).DueAt(time.Time{}, now) {
		t.Fatal("zero interval disables scheduling")
	}
}

func tags(ss []Snapshot) []string {
	var o []string
	for _, s := range ss {
		o = append(o, s.Tag)
	}
	return o
}
