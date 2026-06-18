// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSnapshotter records each Snapshot call and writes a manifest into the
// backing fakeOCI under an RFC3339 tag at the controller's clock, so the real
// ociRegistry can then list and GC it.
type fakeSnapshotter struct {
	f    *fakeOCI
	now  func() time.Time
	mu   sync.Mutex
	tags []string
	err  error
}

func (s *fakeSnapshotter) Snapshot(_ context.Context, tg Target) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	tag := s.now().UTC().Format(time.RFC3339)
	s.f.put(tg.Repo, tag, []byte(`{"schemaVersion":2}`))
	s.mu.Lock()
	s.tags = append(s.tags, tag)
	s.mu.Unlock()
	return tag, nil
}

// TestControllerEndToEnd drives backupAndPrune across many days against the real
// ociRegistry over httptest, asserting GC keeps exactly the policy's keep-set.
func TestControllerEndToEnd(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/e2e"

	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	snap := &fakeSnapshotter{f: f, now: func() time.Time { return clock }}
	reg := NewOCIRegistry(clientFor)

	tg := Target{Volume: "vol-e2e", Repo: repo, Policy: Policy{
		Interval: 24 * time.Hour, KeepLast: 2, KeepDaily: 3,
	}}

	c := New([]Target{tg}, snap, reg, nil, nil, time.Minute, nil)
	c.now = func() time.Time { return clock }

	// Take 6 daily snapshots; after each, retention runs.
	for d := 0; d < 6; d++ {
		clock = time.Date(2026, 1, 1+d, 12, 0, 0, 0, time.UTC)
		if err := c.backupAndPrune(context.Background(), tg); err != nil {
			t.Fatalf("day %d: %v", d, err)
		}
	}

	// Policy keepLast=2 + keepDaily=3 over 6 distinct days ⇒ 3 survivors
	// (the 3 newest days; keepLast's 2 newest overlap the daily set).
	snaps, err := reg.ListSnapshots(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("after GC want 3 surviving snapshots, got %d: %v", len(snaps), tagsOf(snaps))
	}
	// The survivors must be the three newest days.
	want := map[string]bool{
		time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC).Format(time.RFC3339): true,
		time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC).Format(time.RFC3339): true,
		time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC).Format(time.RFC3339): true,
	}
	for _, s := range snaps {
		if !want[s.Tag] {
			t.Fatalf("unexpected survivor %q", s.Tag)
		}
	}
}

func tagsOf(ss []Snapshot) []string {
	var o []string
	for _, s := range ss {
		o = append(o, s.Tag)
	}
	return o
}

func TestControllerSnapshotError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	snap := &fakeSnapshotter{f: f, now: time.Now, err: errors.New("snap fail")}
	c := New(nil, snap, NewOCIRegistry(clientFor), nil, nil, time.Minute, nil)
	tg := Target{Volume: "v", Repo: "r", Policy: Policy{Interval: time.Hour, KeepLast: 1}}
	if err := c.backupAndPrune(context.Background(), tg); err == nil {
		t.Fatal("expected snapshot error to propagate")
	}
}

func TestControllerListError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	f.failList = "r-list"
	snap := &fakeSnapshotter{f: f, now: time.Now}
	c := New(nil, snap, NewOCIRegistry(clientFor), nil, nil, time.Minute, nil)
	tg := Target{Volume: "v", Repo: "r-list", Policy: Policy{Interval: time.Hour, KeepLast: 1}}
	if err := c.backupAndPrune(context.Background(), tg); err == nil {
		t.Fatal("expected list error to propagate")
	}
}

func TestControllerDeleteErrorIsLoggedNotFatal(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "r-del"
	f.failDelete = repo
	// Pre-seed two old snapshots so retention wants to delete one.
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	older := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	f.put(repo, old, []byte(`{"schemaVersion":2}`))
	f.put(repo, older, []byte(`{"schemaVersion":2}`))

	clock := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	snap := &fakeSnapshotter{f: f, now: func() time.Time { return clock }}
	c := New(nil, snap, NewOCIRegistry(clientFor), nil, nil, time.Minute, nil)
	c.now = func() time.Time { return clock }
	tg := Target{Volume: "v", Repo: repo, Policy: Policy{Interval: time.Hour, KeepLast: 1}}
	// Delete fails server-side but backupAndPrune must NOT return an error.
	if err := c.backupAndPrune(context.Background(), tg); err != nil {
		t.Fatalf("delete failure should be logged, not fatal: %v", err)
	}
}

// recordingLeader and storeErr drive the reconcile branches.
type toggleLeader struct{ v bool }

func (l *toggleLeader) IsLeader() bool { return l.v }

type errStore struct {
	getErr, setErr error
	t              time.Time
}

func (s errStore) Get(context.Context, string) (time.Time, error) { return s.t, s.getErr }
func (s errStore) Set(context.Context, string, time.Time) error   { return s.setErr }

func TestReconcileFollowerSkips(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	snap := &fakeSnapshotter{f: f, now: time.Now}
	c := New(
		[]Target{{Volume: "v", Repo: "r", Policy: Policy{Interval: time.Hour, KeepLast: 1}}},
		snap, NewOCIRegistry(clientFor), &toggleLeader{v: false}, nil, time.Millisecond, nil,
	)
	// Run a single tick by cancelling right after one fires.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)
	snap.mu.Lock()
	n := len(snap.tags)
	snap.mu.Unlock()
	if n != 0 {
		t.Fatalf("follower must not snapshot, took %d", n)
	}
}

func TestReconcileStoreGetError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	snap := &fakeSnapshotter{f: f, now: time.Now}
	c := New(
		[]Target{{Volume: "v", Repo: "r", Policy: Policy{Interval: time.Hour, KeepLast: 1}}},
		snap, NewOCIRegistry(clientFor), nil, errStore{getErr: errors.New("get boom")}, time.Minute, nil,
	)
	c.reconcile(context.Background()) // get error -> target skipped, no panic
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.tags) != 0 {
		t.Fatal("store Get error should skip the target")
	}
}

func TestReconcileStoreSetError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := &fakeSnapshotter{f: f, now: func() time.Time { return clock }}
	c := New(
		[]Target{{Volume: "v", Repo: "r", Policy: Policy{Interval: time.Hour, KeepLast: 1}}},
		snap, NewOCIRegistry(clientFor), nil, errStore{setErr: errors.New("set boom")}, time.Minute, nil,
	)
	c.now = func() time.Time { return clock }
	c.reconcile(context.Background()) // snapshot succeeds, Set fails -> logged
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.tags) != 1 {
		t.Fatalf("snapshot should have run once, got %d", len(snap.tags))
	}
}

func TestReconcileNotDueSkips(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := &fakeSnapshotter{f: f, now: func() time.Time { return clock }}
	store := NewMemStore()
	_ = store.Set(context.Background(), "v", clock) // just snapshotted
	c := New(
		[]Target{{Volume: "v", Repo: "r", Policy: Policy{Interval: time.Hour, KeepLast: 1}}},
		snap, NewOCIRegistry(clientFor), AlwaysLeader{}, store, time.Minute, nil,
	)
	c.now = func() time.Time { return clock.Add(time.Minute) } // 1min < 1h interval
	c.reconcile(context.Background())
	snap.mu.Lock()
	defer snap.mu.Unlock()
	if len(snap.tags) != 0 {
		t.Fatal("not-due target should be skipped")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	c := New(nil, &fakeSnapshotter{}, nil, AlwaysLeader{}, nil, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run should return context.Canceled, got %v", err)
	}
}

func TestRunTicksReconcile(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := &fakeSnapshotter{f: f, now: func() time.Time { return clock }}
	c := New(
		[]Target{{Volume: "v", Repo: "r", Policy: Policy{Interval: time.Nanosecond, KeepLast: 1}}},
		snap, NewOCIRegistry(clientFor), nil, nil, time.Millisecond, nil,
	)
	c.now = func() time.Time { return clock }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx)
	snap.mu.Lock()
	n := len(snap.tags)
	snap.mu.Unlock()
	if n == 0 {
		t.Fatal("expected at least one tick to snapshot")
	}
}
