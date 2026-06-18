// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"sync"
	"time"
)

// Store persists the last successful snapshot time per volume so the schedule
// survives a controller restart or leader handover. A zero time means "never
// snapshotted" (always due). The default is the in-memory [MemStore]; a
// production deployment backs it with etcd — keying e.g.
// /weft/volume-backup/last/<volume> to an RFC3339 time — so a new leader resumes
// the cadence instead of re-snapshotting everything on takeover. Get/Set take a
// context so an etcd-backed implementation can honour deadlines/cancellation.
type Store interface {
	// Get returns the last snapshot time for vol, or the zero time if none.
	Get(ctx context.Context, vol string) (time.Time, error)
	// Set records t as vol's last snapshot time.
	Set(ctx context.Context, vol string, t time.Time) error
}

// MemStore is the default in-memory Store. It is safe for concurrent use but is
// NOT durable: last-snapshot times are lost on restart, so a fresh process
// re-snapshots every target on its first tick. Use it for single-node / tests;
// for HA, supply an etcd-backed Store to New.
type MemStore struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{last: map[string]time.Time{}}
}

// Get returns the stored time for vol, or the zero time when absent.
func (m *MemStore) Get(_ context.Context, vol string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last[vol], nil
}

// Set records t as vol's last snapshot time.
func (m *MemStore) Set(_ context.Context, vol string, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last[vol] = t
	return nil
}
