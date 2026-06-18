// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

// Package weftvolumebackup is a SKETCH of the weft orchestration controller that
// automates OCI snapshots + retention for go-volumes volumes. The retention
// algorithm (this file) is real and tested; the control loop and the
// Snapshotter/Registry seams (backup.go) are a skeleton with reference wiring
// left as TODO. See README.md.
package weftvolumebackup

import (
	"sort"
	"time"
)

// Policy is the per-target backup schedule and grandfather-father-son retention.
// The zero value backs up nothing and keeps nothing; set at least Interval and
// one Keep* field. A snapshot is retained if it falls in ANY keep bucket.
type Policy struct {
	// Interval is the minimum time between snapshots of a target. A snapshot is
	// taken when now - lastSnapshot >= Interval.
	Interval time.Duration

	// KeepLast keeps the N most recent snapshots regardless of age.
	KeepLast int
	// KeepDaily keeps the most recent snapshot of each of the last N distinct
	// calendar days that have a snapshot. Weekly/Monthly/Yearly are analogous
	// over ISO weeks, months, and years.
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
}

// Snapshot is one immutable backup of a target — an OCI tag and when it was
// created. Tag is the registry tag (e.g. an RFC3339 timestamp); CreatedAt drives
// retention bucketing.
type Snapshot struct {
	Tag       string
	CreatedAt time.Time
}

// DueAt reports whether a new snapshot is due given the most recent snapshot
// time (zero time = never snapshotted, so always due). Interval <= 0 disables
// scheduling (never due).
func (p Policy) DueAt(lastSnapshot, now time.Time) bool {
	if p.Interval <= 0 {
		return false
	}
	return now.Sub(lastSnapshot) >= p.Interval
}

// Retain partitions snapshots into the set to KEEP and the set to EXPIRE under
// the policy, evaluated as of now. It is pure and deterministic: a snapshot is
// kept iff it wins at least one of the last/daily/weekly/monthly/yearly buckets.
// Input order is irrelevant; the returned slices are newest-first.
func (p Policy) Retain(snapshots []Snapshot, now time.Time) (keep, expire []Snapshot) {
	snaps := append([]Snapshot(nil), snapshots...)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })

	kept := make(map[string]bool, len(snaps))

	// keep-last: the N newest outright.
	for i := 0; i < p.KeepLast && i < len(snaps); i++ {
		kept[snaps[i].Tag] = true
	}

	// Periodic buckets: walking newest→oldest, the FIRST snapshot seen in a
	// given period is that period's survivor; keep one survivor per period up to
	// the bucket's count.
	keepPeriodic := func(count int, key func(time.Time) string) {
		if count <= 0 {
			return
		}
		seen := make(map[string]bool)
		for _, s := range snaps {
			k := key(s.CreatedAt)
			if seen[k] {
				continue // an older snapshot in the same period — skip
			}
			seen[k] = true
			kept[s.Tag] = true
			if len(seen) >= count {
				break
			}
		}
	}
	keepPeriodic(p.KeepDaily, dayKey)
	keepPeriodic(p.KeepWeekly, weekKey)
	keepPeriodic(p.KeepMonthly, monthKey)
	keepPeriodic(p.KeepYearly, yearKey)

	for _, s := range snaps {
		if kept[s.Tag] {
			keep = append(keep, s)
		} else {
			expire = append(expire, s)
		}
	}
	return keep, expire
}

// Period keys (UTC) used for bucketing. Same key ⇒ same period.
func dayKey(t time.Time) string   { return t.UTC().Format("2006-01-02") }
func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }
func yearKey(t time.Time) string  { return t.UTC().Format("2006") }
func weekKey(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") + "-W" + itoa2(w)
}

func itoa2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
