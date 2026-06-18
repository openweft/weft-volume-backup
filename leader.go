// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

// This file provides the two reference Leader adapters over a lease source. The
// Leader interface itself lives in backup.go (it is the controller's seam).
//
// Integration point: wire IsLeader to a real lease/election so exactly one node
// runs backups. Two natural sources in the weft stack:
//
//   - etcd concurrency election (go.etcd.io/etcd/client/v3/concurrency): hold a
//     campaign; IsLeader reports whether this node currently owns the leader key.
//   - replica-ha's Coordinator: report whether this replica is the primary.
//
// Neither is pulled into the core module — keeping the etcd/replica-ha
// dependency behind this seam keeps the dependency graph lean. A deployment
// constructs its own adapter, e.g.:
//
//	led := FuncLeader(func() bool { return election.IsLeader() })
//
// and passes it to New. Because snapshot tags are content-addressed, an
// accidental two-leader overlap is merely wasteful, never corrupting.

// AlwaysLeader is the single-node Leader: it always reports leadership. It is
// the right choice when only one controller ever runs (and is equivalent to
// passing a nil Leader to New).
type AlwaysLeader struct{}

// IsLeader always returns true.
func (AlwaysLeader) IsLeader() bool { return true }

// FuncLeader adapts a predicate to the Leader interface, so a deployment can
// back leadership with an etcd election or replica-ha Coordinator without this
// package importing either. The function is called on every tick and must be
// safe for concurrent use.
type FuncLeader func() bool

// IsLeader reports the wrapped predicate's current value.
func (f FuncLeader) IsLeader() bool { return f() }
