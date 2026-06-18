// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-volumes/oci/registry"
)

// --- leader.go ---

func TestLeaders(t *testing.T) {
	if !(AlwaysLeader{}).IsLeader() {
		t.Fatal("AlwaysLeader must report leadership")
	}
	called := false
	f := FuncLeader(func() bool { called = true; return false })
	if f.IsLeader() || !called {
		t.Fatal("FuncLeader must call and return the predicate")
	}
}

// --- snapshot.go (ociSnapshotter + the public constructor) ---

func dummyClientFor(repo string) *registry.Client {
	return &registry.Client{BaseURL: "http://example.invalid", Repository: repo}
}

func TestNewOCISnapshotter(t *testing.T) {
	committed := false
	access := func(_ context.Context, _ Target) (Committer, func(), error) {
		return CommitterFunc(func(_ context.Context, _ *registry.Client, _ string) (string, error) {
			committed = true
			return "sha256:x", nil
		}), nil, nil
	}
	s := NewOCISnapshotter(access, dummyClientFor)
	tag, err := s.Snapshot(context.Background(), Target{Volume: "v", Repo: "r"})
	if err != nil || tag == "" || !committed {
		t.Fatalf("Snapshot via constructor: tag=%q err=%v committed=%v", tag, err, committed)
	}
	if _, e := time.Parse(time.RFC3339, tag); e != nil {
		t.Fatalf("tag is not RFC3339: %q", tag)
	}
}

func TestOCISnapshotterSuccess(t *testing.T) {
	var gotRef string
	cleaned := false
	access := func(_ context.Context, _ Target) (Committer, func(), error) {
		return CommitterFunc(func(_ context.Context, _ *registry.Client, ref string) (string, error) {
			gotRef = ref
			return "sha256:deadbeef", nil
		}), func() { cleaned = true }, nil
	}
	fixed := time.Date(2026, 6, 17, 8, 30, 0, 0, time.UTC)
	s := &ociSnapshotter{access: access, clientFor: dummyClientFor, now: func() time.Time { return fixed }}

	tag, err := s.Snapshot(context.Background(), Target{Volume: "v", Repo: "r"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if want := fixed.Format(time.RFC3339); tag != want || gotRef != want {
		t.Fatalf("tag=%q ref=%q, want %q", tag, gotRef, want)
	}
	if !cleaned {
		t.Fatal("cleanup was not called")
	}
}

func TestOCISnapshotterAccessError(t *testing.T) {
	s := &ociSnapshotter{
		access:    func(_ context.Context, _ Target) (Committer, func(), error) { return nil, nil, errors.New("open boom") },
		clientFor: dummyClientFor,
		now:       time.Now,
	}
	if _, err := s.Snapshot(context.Background(), Target{Volume: "v", Repo: "r"}); err == nil {
		t.Fatal("expected access error")
	}
}

func TestOCISnapshotterCommitError(t *testing.T) {
	// nil cleanup is also exercised here (the defer-nil branch).
	s := &ociSnapshotter{
		access: func(_ context.Context, _ Target) (Committer, func(), error) {
			return CommitterFunc(func(_ context.Context, _ *registry.Client, _ string) (string, error) {
				return "", errors.New("commit boom")
			}), nil, nil
		},
		clientFor: dummyClientFor,
		now:       time.Now,
	}
	if _, err := s.Snapshot(context.Background(), Target{Volume: "v", Repo: "r"}); err == nil {
		t.Fatal("expected commit error")
	}
}

// --- policy.go periodic buckets (weekly/monthly/yearly) + itoa2 ---

func TestRetainPeriodicBuckets(t *testing.T) {
	// One snapshot at noon on the 1st of each month, Jan 2024 .. Jun 2026.
	var snaps []Snapshot
	for _, ym := range monthsRange(2024, 1, 2026, 6) {
		ts := time.Date(ym[0], time.Month(ym[1]), 1, 12, 0, 0, 0, time.UTC)
		snaps = append(snaps, Snapshot{Tag: ts.Format(time.RFC3339), CreatedAt: ts})
	}
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	p := Policy{KeepMonthly: 3, KeepYearly: 2}
	keep, expire := p.Retain(snaps, now)
	if len(keep)+len(expire) != len(snaps) {
		t.Fatalf("keep+expire=%d != %d", len(keep)+len(expire), len(snaps))
	}
	// Monthly picks 2026-06/05/04; yearly adds 2026 (already in) + 2025 newest
	// (2025-12) → 4 distinct survivors.
	if len(keep) != 4 {
		t.Fatalf("keep=%d want 4: %v", len(keep), tagsOf(keep))
	}
	mustKeep(t, keep, "2026-06-01T12:00:00Z", "2026-05-01T12:00:00Z", "2026-04-01T12:00:00Z", "2025-12-01T12:00:00Z")
}

func TestRetainWeekly(t *testing.T) {
	// Daily snapshots across ~4 ISO weeks; KeepWeekly=2 keeps one per of the 2
	// most recent weeks (exercises weekKey).
	var snaps []Snapshot
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC) // Thu, ISO week 1
	for d := 0; d < 28; d++ {
		ts := base.AddDate(0, 0, d)
		snaps = append(snaps, Snapshot{Tag: ts.Format(time.RFC3339), CreatedAt: ts})
	}
	now := base.AddDate(0, 0, 28)
	keep, _ := (Policy{KeepWeekly: 2}).Retain(snaps, now)
	if len(keep) != 2 {
		t.Fatalf("weekly keep=%d want 2: %v", len(keep), tagsOf(keep))
	}
}

func TestItoa2(t *testing.T) {
	// Both branches: single-digit (zero-padded) and two-digit.
	for _, c := range []struct {
		n    int
		want string
	}{{0, "00"}, {5, "05"}, {9, "09"}, {10, "10"}, {42, "42"}, {53, "53"}} {
		if got := itoa2(c.n); got != c.want {
			t.Errorf("itoa2(%d)=%q want %q", c.n, got, c.want)
		}
	}
}

// --- backup.go reconcile: a due target whose backup fails is logged and
// skipped, and crucially its last-snapshot time is NOT persisted. ---

func TestReconcileBackupError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	snap := &fakeSnapshotter{f: f, now: time.Now, err: errors.New("snap boom")}
	reg := NewOCIRegistry(clientFor)
	store := NewMemStore()
	tg := Target{Volume: "v", Repo: "vol/x", Policy: Policy{Interval: time.Hour, KeepLast: 1}}

	c := New([]Target{tg}, snap, reg, nil, store, time.Hour, nil)
	c.reconcile(context.Background()) // empty store ⇒ due ⇒ snapshot errors ⇒ logged, continue

	if ts, _ := store.Get(context.Background(), "v"); !ts.IsZero() {
		t.Fatalf("store must stay unset when backup fails, got %v", ts)
	}
}

// --- registry.go createdAt: an opaque tag that lists fine but whose manifest
// GET fails ⇒ createdAt's get-manifest-error branch. ---

func TestCreatedAtGetManifestError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "vol/x"
	f.put(repo, "opaque", []byte(`{"schemaVersion":2}`)) // listed...
	f.failGet = repo                                     // ...but manifest GET 500s

	var sawErr bool
	r := &ociRegistry{clientFor: clientFor, onUnbucketable: func(_, _ string, err error) {
		if err != nil {
			sawErr = true
		}
	}}
	snaps, err := r.ListSnapshots(context.Background(), repo)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 || !sawErr {
		t.Fatalf("get-manifest-error tag should be skipped via onUnbucketable; snaps=%v sawErr=%v", snaps, sawErr)
	}
}

// --- helpers ---

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func mustKeep(t *testing.T, keep []Snapshot, tags ...string) {
	t.Helper()
	got := tagsOf(keep)
	for _, want := range tags {
		if !contains(got, want) {
			t.Fatalf("missing expected survivor %q in %v", want, got)
		}
	}
}

func monthsRange(y0, m0, y1, m1 int) [][2]int {
	var out [][2]int
	for y, m := y0, m0; y < y1 || (y == y1 && m <= m1); {
		out = append(out, [2]int{y, m})
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return out
}
