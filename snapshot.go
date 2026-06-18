// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"fmt"
	"time"

	"github.com/go-volumes/oci/registry"
)

// Committer snapshots one volume's current state into the immutable OCI artifact
// tagged ref on dst, returning the new manifest digest. It is exactly the shape
// of both go-volumes/oci snapshot mechanisms:
//
//   - oci.Overlay.Commit(ctx, dst, ref)        — delta-commit an OCI-overlay volume;
//   - func(ctx,dst,ref){ return oci.Freeze(ctx, ro, dst, ref, opts) } — freeze a
//     read-only view of any volume.
//
// Keeping the Snapshotter behind this one-method seam decouples this package
// from how a volume is opened: the weft-driver-qemu GoVolume driver supplies a
// Committer (its CreateBackup path) without this module importing the driver.
type Committer interface {
	Commit(ctx context.Context, dst *registry.Client, ref string) (string, error)
}

// CommitterFunc adapts a function to Committer.
type CommitterFunc func(ctx context.Context, dst *registry.Client, ref string) (string, error)

// Commit calls f.
func (f CommitterFunc) Commit(ctx context.Context, dst *registry.Client, ref string) (string, error) {
	return f(ctx, dst, ref)
}

// VolumeAccess opens a Committer for a target's current volume state and returns
// it together with a cleanup the snapshotter calls when done (may be nil). The
// reference wiring is an adapter over the weft-driver-qemu GoVolume driver:
// open the volume's overlay/image and return its Commit/Freeze as the Committer.
// Injecting it here is what keeps this package free of any qemu-driver import.
type VolumeAccess func(ctx context.Context, target Target) (c Committer, cleanup func(), err error)

// ociSnapshotter implements Snapshotter: it opens a target's volume via the
// injected VolumeAccess, commits/freezes it to a fresh timestamped OCI tag, and
// returns that tag. Tags are RFC3339-UTC timestamps so ListSnapshots can recover
// each snapshot's time from the tag alone (no per-tag manifest GET).
type ociSnapshotter struct {
	access    VolumeAccess
	clientFor ClientFor
	now       func() time.Time
}

// NewOCISnapshotter builds a Snapshotter. access opens the per-target volume as a
// Committer; clientFor maps a repository to the destination registry client
// (shared with NewOCIRegistry so snapshots and GC target the same registry).
func NewOCISnapshotter(access VolumeAccess, clientFor ClientFor) Snapshotter {
	return &ociSnapshotter{access: access, clientFor: clientFor, now: time.Now}
}

// Snapshot opens target's volume, commits it to a new RFC3339-timestamped tag in
// target.Repo, and returns the tag.
func (s *ociSnapshotter) Snapshot(ctx context.Context, target Target) (string, error) {
	committer, cleanup, err := s.access(ctx, target)
	if err != nil {
		return "", fmt.Errorf("weft-volume-backup: open volume %q: %w", target.Volume, err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	tag := s.now().UTC().Format(time.RFC3339)
	dst := s.clientFor(target.Repo)
	if _, err := committer.Commit(ctx, dst, tag); err != nil {
		return "", fmt.Errorf("weft-volume-backup: snapshot %q -> %s: %w", target.Volume, tag, err)
	}
	return tag, nil
}
