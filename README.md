# weft-volume-backup

[![ci](https://github.com/openweft/weft-volume-backup/actions/workflows/ci.yml/badge.svg)](https://github.com/openweft/weft-volume-backup/actions/workflows/ci.yml)

A weft orchestration controller that **automates OCI snapshots and retention**
for [go-volumes](https://github.com/go-volumes) volumes. It is the *policy* layer
on top of the generic OCI mechanism (`go-volumes/oci` freeze / overlay-commit) —
the same split as the rest of the stack: go-volumes provides the mechanism,
weft decides *when* and *how long to keep*.

Pure Go (`CGO_ENABLED=0`), stdlib + `go-volumes/oci` + cobra only, BSD-3-Clause,
cross-built on all six 64-bit targets (amd64/arm64/riscv64/loong64/ppc64le/s390x)
with a per-function 100%-coverage gate.

## Why it is weft-side (and small)

Backing a volume up is already a one-liner mechanism (`oci.Freeze` or
`overlay.Commit` → an immutable, content-addressed, delta-deduped OCI tag — see
the `weft-driver-qemu` `GoVolume` driver). What is missing is purely
**orchestration**:

- **When** — a schedule per volume / per storage class.
- **How many to keep** — retention (grandfather-father-son), and GC of the
  expired OCI tags from the registry (weft-zot).
- **Where / who** — one leader runs the backups (reuse weft's etcd or the
  `replica-ha` `Coordinator`), against the cluster registry.

None of that is a new *storage* implementation — it is a controller. Hence: weft.

## Shape

```
                Policy (interval + GFS retention, per target)
                        │
   ┌──────────────── Controller (leader-only) ──────────────────┐
   │  for each target, on schedule:                              │
   │    1. Snapshotter.Snapshot(target)  → new OCI tag (delta)   │  ← go-volumes/oci
   │    2. Registry.ListSnapshots(repo)  → existing tags+times   │  ← OCI /v2 tags/list
   │    3. Policy.Retain(snapshots)      → keep-set / expire-set │  ← pure, tested
   │    4. Registry.Delete(repo, tag)    for each expired        │  ← OCI manifest DELETE
   └────────────────────────────────────────────────────────────┘
```

## Seams (bring-your-own, like Coordinator/Fencer)

```go
// Snapshotter creates one immutable snapshot of a target and returns its tag.
// The reference ociSnapshotter (snapshot.go) opens the volume via an injected
// VolumeAccess and calls the GoVolume driver's CreateBackup (oci.Freeze) or
// oci.Overlay.Commit; the tag is the snapshot's RFC3339-UTC timestamp.
type Snapshotter interface {
    Snapshot(ctx context.Context, target Target) (tag string, err error)
}

// Registry lists and deletes snapshots for retention GC. The reference
// ociRegistry (registry.go) implements it over go-volumes/oci/registry:
// GET /v2/<repo>/tags/list (TagsList) + DELETE /v2/<repo>/manifests/<ref>
// (DeleteManifest) — the only two calls GC adds; push/pull already exist.
type Registry interface {
    ListSnapshots(ctx context.Context, repo string) ([]Snapshot, error)
    Delete(ctx context.Context, repo, tag string) error
}

// Leader gates backups to one node (nil = single-node always-leader). Back it
// with weft's etcd Election or replica-ha's Coordinator.
type Leader interface{ IsLeader() bool }

// Store persists each volume's last-snapshot time so a leader handover does not
// re-snapshot early. MemStore is the default; an etcd-backed Store survives
// restarts.
type Store interface {
    Get(ctx context.Context, vol string) (time.Time, error)
    Set(ctx context.Context, vol string, t time.Time) error
}
```

## Retention (the algorithmic core — `policy.go`)

Grandfather-father-son, the restic/Velero model: keep the last *N*, plus the
last *D* daily, *W* weekly, *M* monthly, *Y* yearly. A snapshot survives if it is
in *any* keep-set; everything else is deleted. Pure, deterministic, fully unit-
testable without a registry — see `Policy.Retain`.

Tags are RFC3339-UTC timestamps, so `ListSnapshots` recovers each snapshot's time
from the tag alone; an opaque tag falls back to the manifest's
`org.opencontainers.image.created` annotation.

## Concrete wiring

| seam          | reference implementation                                                        |
| ------------- | ------------------------------------------------------------------------------- |
| `Snapshotter` | `ociSnapshotter` — `VolumeAccess` → `oci.Freeze` / `oci.Overlay.Commit`         |
| `Registry`    | `ociRegistry` over `go-volumes/oci/registry` (`TagsList` + `DeleteManifest`)    |
| `Leader`      | `AlwaysLeader` / `FuncLeader`; weft etcd or `replica-ha` `Coordinator` in-cluster |
| `Store`       | `MemStore` (default); etcd-backed Store for restart survival                    |

The `weft-driver-qemu` `GoVolume` driver supplies the `VolumeAccess`, so this
module never imports the driver.

## The agent

`cmd/weft-volume-backup agent --config config.json` runs the control loop until
`SIGINT`/`SIGTERM`. With no `VolumeAccess` wired the standalone binary runs
retention-GC only; an integration build injects `volumeAccess` (the qemu driver)
and an etcd `leaderProvider`.

```json
{
  "registry": "https://registry.weft.local",
  "tick": "1m",
  "targets": [
    {"volume": "vol-a", "repo": "volumes/a", "interval": "1h",
     "keepLast": 3, "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 6}
  ]
}
```

## License

BSD-3-Clause © the openweft/weft-volume-backup authors.
