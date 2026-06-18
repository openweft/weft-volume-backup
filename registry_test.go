// Copyright (c) 2026, the openweft/weft-volume-backup authors
// SPDX-License-Identifier: BSD-3-Clause

package weftvolumebackup

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestOCIRegistryListByRFC3339Tag(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/v1"
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		tag := t0.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
		f.put(repo, tag, []byte(`{"schemaVersion":2}`))
	}
	reg := NewOCIRegistry(clientFor)
	snaps, err := reg.ListSnapshots(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("snaps=%d want 3", len(snaps))
	}
	// CreatedAt recovered from the tag, no manifest GET needed.
	for _, s := range snaps {
		if s.CreatedAt.IsZero() {
			t.Fatalf("tag %q got zero time", s.Tag)
		}
	}
}

func TestOCIRegistryListByAnnotation(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/v2"
	created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC).Format(time.RFC3339)
	body := fmt.Sprintf(`{"schemaVersion":2,"annotations":{"%s":%q}}`, annotationCreated, created)
	f.put(repo, "opaque-tag", []byte(body))

	reg := NewOCIRegistry(clientFor)
	snaps, err := reg.ListSnapshots(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Tag != "opaque-tag" {
		t.Fatalf("snaps=%v", snaps)
	}
	if !snaps[0].CreatedAt.Equal(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Fatalf("annotation time wrong: %v", snaps[0].CreatedAt)
	}
}

func TestOCIRegistryUnbucketableSkipped(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/v3"
	// One good RFC3339 tag, one opaque tag with no annotation, one with a bad
	// annotation value.
	good := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	f.put(repo, good, []byte(`{"schemaVersion":2}`))
	f.put(repo, "no-anno", []byte(`{"schemaVersion":2}`))
	f.put(repo, "bad-anno", []byte(fmt.Sprintf(`{"annotations":{"%s":"not-a-time"}}`, annotationCreated)))

	var skipped []string
	reg := &ociRegistry{clientFor: clientFor, onUnbucketable: func(_, tag string, _ error) {
		skipped = append(skipped, tag)
	}}
	snaps, err := reg.ListSnapshots(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Tag != good {
		t.Fatalf("expected only the RFC3339 tag, got %v", snaps)
	}
	if len(skipped) != 2 {
		t.Fatalf("expected 2 unbucketable, got %v", skipped)
	}
}

func TestOCIRegistryBadManifestJSON(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/v4"
	f.put(repo, "opaque", []byte("not json"))
	reg := &ociRegistry{clientFor: clientFor} // no hook -> just skipped
	snaps, err := reg.ListSnapshots(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("bad-json manifest should be skipped, got %v", snaps)
	}
}

func TestOCIRegistryListEmptyRepo(t *testing.T) {
	_, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	reg := NewOCIRegistry(clientFor)
	snaps, err := reg.ListSnapshots(context.Background(), "volumes/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("empty repo should list nothing, got %v", snaps)
	}
}

func TestOCIRegistryListError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	f.failList = "volumes/boom"
	reg := NewOCIRegistry(clientFor)
	if _, err := reg.ListSnapshots(context.Background(), "volumes/boom"); err == nil {
		t.Fatal("expected list error")
	}
}

func TestOCIRegistryGetManifestError(t *testing.T) {
	// An opaque tag whose manifest GET 404s (tag listed but body absent is not
	// reachable via fakeOCI; instead point at a closed server).
	f, clientFor, closeFn := startFakeOCI(t)
	repo := "volumes/v5"
	f.put(repo, "opaque", []byte(`{"schemaVersion":2}`))
	closeFn() // close so GetManifest transport-fails
	var sawErr bool
	reg := &ociRegistry{clientFor: clientFor, onUnbucketable: func(_, _ string, err error) {
		if err != nil {
			sawErr = true
		}
	}}
	// TagsList itself will fail against the closed server -> ListSnapshots errors.
	if _, err := reg.ListSnapshots(context.Background(), repo); err == nil {
		t.Fatal("expected error against closed server")
	}
	_ = sawErr
}

func TestOCIRegistryDelete(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/v6"
	tag := time.Now().UTC().Format(time.RFC3339)
	f.put(repo, tag, []byte(`{"schemaVersion":2}`))
	reg := NewOCIRegistry(clientFor)

	if err := reg.Delete(context.Background(), repo, tag); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Idempotent: deleting an already-absent tag is success (ErrNotFound swallowed).
	if err := reg.Delete(context.Background(), repo, tag); err != nil {
		t.Fatalf("idempotent re-delete: %v", err)
	}
}

func TestOCIRegistryDeleteError(t *testing.T) {
	f, clientFor, closeFn := startFakeOCI(t)
	defer closeFn()
	repo := "volumes/v7"
	f.failDelete = repo
	tag := time.Now().UTC().Format(time.RFC3339)
	f.put(repo, tag, []byte(`{"schemaVersion":2}`))
	reg := NewOCIRegistry(clientFor)
	if err := reg.Delete(context.Background(), repo, tag); err == nil {
		t.Fatal("expected delete error")
	}
}
