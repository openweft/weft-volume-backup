// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-volumes/oci/registry"
)

// ClientFor returns a registry client bound to one OCI repository. A deployment
// supplies it so ociRegistry can address many repos (one per volume) from a
// single base configuration — typically by cloning a template Client and setting
// Repository. It must not return nil.
type ClientFor func(repo string) *registry.Client

// ociRegistry implements the Registry seam over go-volumes/oci/registry, using
// the TagsList + DeleteManifest GC primitives. It lists a repository's snapshot
// tags and resolves each tag's creation time, and deletes expired snapshots.
//
// CreatedAt resolution, in order:
//  1. parse the tag itself as an RFC3339 timestamp (the convention this package's
//     ociSnapshotter uses — see snapshot.go), which needs no network round-trip;
//  2. failing that, fetch the manifest and read its
//     "org.opencontainers.image.created" annotation;
//  3. failing that, the tag is skipped for retention purposes (it cannot be
//     bucketed) but reported via the optional onUnbucketable hook.
//
// This keeps the common case (timestamp tags) free of per-tag GETs while still
// supporting opaque tags that carry their time in the manifest.
type ociRegistry struct {
	clientFor ClientFor

	// now and onUnbucketable are test/observability seams.
	onUnbucketable func(repo, tag string, err error)
}

// NewOCIRegistry builds a Registry backed by go-volumes/oci/registry. clientFor
// maps a repository name to a configured *registry.Client (auth, base URL).
func NewOCIRegistry(clientFor ClientFor) Registry {
	return &ociRegistry{clientFor: clientFor}
}

// annotatedManifest is the slice of an OCI manifest ociRegistry reads to recover
// a snapshot's creation time when the tag is not itself a timestamp.
type annotatedManifest struct {
	Annotations map[string]string `json:"annotations"`
}

// annotationCreated is the standard OCI annotation carrying an RFC3339 build time.
const annotationCreated = "org.opencontainers.image.created"

// ListSnapshots returns every snapshot tag in repo with its creation time. Tags
// whose time cannot be resolved are omitted from the result (and surfaced via
// the onUnbucketable hook when set) so retention never expires what it cannot
// reason about.
func (o *ociRegistry) ListSnapshots(ctx context.Context, repo string) ([]Snapshot, error) {
	cl := o.clientFor(repo)
	tags, err := cl.TagsList(ctx)
	if err != nil {
		return nil, fmt.Errorf("weft-volume-backup: list tags for %q: %w", repo, err)
	}
	var snaps []Snapshot
	for _, tag := range tags {
		t, err := o.createdAt(ctx, cl, tag)
		if err != nil {
			if o.onUnbucketable != nil {
				o.onUnbucketable(repo, tag, err)
			}
			continue
		}
		snaps = append(snaps, Snapshot{Tag: tag, CreatedAt: t})
	}
	return snaps, nil
}

// createdAt resolves a tag's creation time: RFC3339 tag first, then the manifest
// annotation.
func (o *ociRegistry) createdAt(ctx context.Context, cl *registry.Client, tag string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, tag); err == nil {
		return t, nil
	}
	_, body, err := cl.GetManifest(ctx, tag)
	if err != nil {
		return time.Time{}, fmt.Errorf("get manifest %q: %w", tag, err)
	}
	var m annotatedManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return time.Time{}, fmt.Errorf("decode manifest %q: %w", tag, err)
	}
	created := m.Annotations[annotationCreated]
	if created == "" {
		return time.Time{}, fmt.Errorf("manifest %q has no %s annotation and tag is not RFC3339", tag, annotationCreated)
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return time.Time{}, fmt.Errorf("manifest %q %s=%q: %w", tag, annotationCreated, created, err)
	}
	return t, nil
}

// Delete removes the snapshot tagged tag from repo. A tag that is already gone
// (the registry's ErrNotFound) is treated as success — GC is idempotent.
func (o *ociRegistry) Delete(ctx context.Context, repo, tag string) error {
	cl := o.clientFor(repo)
	if err := cl.DeleteManifest(ctx, tag); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("weft-volume-backup: delete %q/%q: %w", repo, tag, err)
	}
	return nil
}
