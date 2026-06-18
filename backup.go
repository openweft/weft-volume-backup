// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"log/slog"
	"time"
)

// Target is one volume to back up: its identity, the OCI repository its
// snapshots live under, and the retention policy. (A storage-class indirection —
// many volumes sharing one Policy — is a trivial extension over this.)
type Target struct {
	// Volume is the volume identity passed to the Snapshotter (e.g. a UUID).
	Volume string
	// Repo is the OCI repository under which this target's snapshot tags live
	// (e.g. "volumes/<uuid>"), used by the Registry for listing + GC.
	Repo string
	// Policy is the schedule + retention for this target.
	Policy Policy
}

// Snapshotter creates one immutable snapshot of a target and returns its tag.
// The reference implementation calls the GoVolume driver's CreateBackup
// (oci.Freeze of the current volume) or oci.Overlay.Commit (for an OCI-overlay
// volume) — both yield a delta-deduped, content-addressed OCI tag.
type Snapshotter interface {
	Snapshot(ctx context.Context, target Target) (tag string, err error)
}

// Registry lists and deletes snapshots for retention GC. Implement it over the
// OCI Distribution v2 API: GET /v2/<repo>/tags/list and
// DELETE /v2/<repo>/manifests/<ref>. These two calls are the only additions the
// go-volumes/oci registry client needs for GC — push/pull already exist.
type Registry interface {
	ListSnapshots(ctx context.Context, repo string) ([]Snapshot, error)
	Delete(ctx context.Context, repo, tag string) error
}

// Leader gates backups to a single node so two controllers do not both back up
// (snapshots are content-addressed so an overlap is merely wasteful, not
// corrupting — but one leader is cleaner). Back it with weft's etcd or
// replica-ha's Coordinator. A nil Leader means "always run" (single-node).
type Leader interface {
	IsLeader() bool
}

// Controller periodically snapshots each Target on its Policy schedule and
// enforces retention by deleting expired snapshots from the Registry. It runs
// only while Leader reports leadership. Construct with New and drive with Run.
type Controller struct {
	targets     []Target
	snapshotter Snapshotter
	registry    Registry
	leader      Leader
	store       Store
	log         *slog.Logger
	tick        time.Duration
	now         func() time.Time // swappable in tests
}

// New builds a Controller. tick is how often the loop wakes to re-evaluate
// schedules (e.g. a minute); the actual snapshot cadence is each Target's
// Policy.Interval. A nil leader means single-node (always leader); a nil store
// uses an in-memory [MemStore] (last-snapshot times are lost on restart — pass
// an etcd-backed Store to survive a leader handover); a nil log discards.
func New(targets []Target, snap Snapshotter, reg Registry, leader Leader, store Store, tick time.Duration, log *slog.Logger) *Controller {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if store == nil {
		store = NewMemStore()
	}
	return &Controller{
		targets:     targets,
		snapshotter: snap,
		registry:    reg,
		leader:      leader,
		store:       store,
		log:         log,
		tick:        tick,
		now:         time.Now,
	}
}

// Run evaluates the schedule every tick until ctx is cancelled. On each tick, if
// this node is the leader, every due target is snapshotted and retention is
// enforced. Run returns ctx.Err() on cancellation.
func (c *Controller) Run(ctx context.Context) error {
	t := time.NewTicker(c.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if c.leader != nil && !c.leader.IsLeader() {
				continue // a follower never backs up
			}
			c.reconcile(ctx)
		}
	}
}

// reconcile snapshots every due target then prunes it. Errors are logged and
// skipped — one target's failure must not stall the others.
func (c *Controller) reconcile(ctx context.Context) {
	for _, tg := range c.targets {
		last, err := c.store.Get(ctx, tg.Volume)
		if err != nil {
			c.log.Error("last-snapshot lookup failed", "volume", tg.Volume, "err", err)
			continue
		}
		if !tg.Policy.DueAt(last, c.now()) {
			continue
		}
		if err := c.backupAndPrune(ctx, tg); err != nil {
			c.log.Error("backup/prune failed", "volume", tg.Volume, "err", err)
			continue
		}
		if err := c.store.Set(ctx, tg.Volume, c.now()); err != nil {
			c.log.Error("last-snapshot persist failed", "volume", tg.Volume, "err", err)
		}
	}
}

// backupAndPrune takes one snapshot of tg and deletes the ones its policy no
// longer keeps. Exposed-shaped so a test can drive a single target directly.
func (c *Controller) backupAndPrune(ctx context.Context, tg Target) error {
	tag, err := c.snapshotter.Snapshot(ctx, tg)
	if err != nil {
		return err
	}
	c.log.Info("snapshot taken", "volume", tg.Volume, "tag", tag)

	snaps, err := c.registry.ListSnapshots(ctx, tg.Repo)
	if err != nil {
		return err
	}
	_, expire := tg.Policy.Retain(snaps, c.now())
	for _, s := range expire {
		if err := c.registry.Delete(ctx, tg.Repo, s.Tag); err != nil {
			// Log and keep going: a stuck delete must not block newer GC.
			c.log.Warn("snapshot GC delete failed", "repo", tg.Repo, "tag", s.Tag, "err", err)
			continue
		}
		c.log.Info("expired snapshot deleted", "repo", tg.Repo, "tag", s.Tag)
	}
	return nil
}

// The deferred wiring the sketch described is now concrete:
//   - ociSnapshotter (snapshot.go): a Snapshotter that commits/freezes a volume
//     to a timestamped OCI tag via go-volumes/oci, with volume access injected.
//   - ociRegistry (registry.go): a Registry over go-volumes/oci/registry using
//     TagsList (GET /v2/<repo>/tags/list) + DeleteManifest
//     (DELETE /v2/<repo>/manifests/<ref>).
//   - Store (store.go): persistent last-snapshot state behind an interface, with
//     an in-memory default and a documented etcd-backed option.
//   - Leader (leader.go): AlwaysLeader / FuncLeader adapters over a lease source
//     (etcd election / replica-ha Coordinator).
//   - cmd/weft-volume-backup: a cobra agent wiring targets + the impls above.
