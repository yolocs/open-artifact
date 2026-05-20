# open-artifact — Vision

## The pitch

A lightweight, multi-format artifact registry that uses **any
`gocloud.dev/blob` bucket** as its sole storage backend. Think of it as a
small, open-source drop-in for Artifactory or Nexus — for teams who already
have an object store (S3, GCS, Azure Blob) and don't want to run Postgres +
a metadata service to put a package registry in front of it.

Speak `pip` and `npm` on the front; write plain objects to a bucket on the
back. The bucket's directory tree *is* the index — no database, no separate
search/metadata store, no OCI registry.

## Why a rewrite (and why not OCI)

`yolocs/ocifactory` proved the multi-format-proxy idea but bet on an OCI
registry as the only backend. In practice that was the wrong substrate:
OCI's manifest/blob model fights the "files under a path" shape that package
ecosystems assume, and it ties operators to running (or renting) a registry
just to store tarballs. `gocloud.dev/blob` is the right primitive — a
content store with one interface over S3 / GCS / Azure / filesystem /
in-memory. open-artifact keeps ocifactory's formats, CLI philosophy, and
testing bar, and swaps the backend.

## Architecture in one breath

```
package manager ──HTTP──▶ surface (pypi|npm) ──▶ core.Store ──▶ blob.Bucket ──▶ S3/GCS/Azure/fs/mem
                              │                      ▲
                              └── upstream client ───┘ (cache-through proxy)
```

- **`pkg/core`** — the substrate. Four value-type nouns
  (`Package → Version → File`, `Tag → Version`), a `Format` enum, a
  scope-blind `Store` interface, a `Meta` envelope (baseline + opaque `Ext`),
  and sentinel errors. Knows nothing about HTTP or upstreams.
- **`pkg/core/blobstore`** — `core.Store` over a `*blob.Bucket`. Owns the
  on-bucket path scheme, the `.meta`/`.tags` dot-file conventions, streaming
  uploads with rolling SHA256, in-process tag mutexes, and the
  facade-transparent SignedURL / Stat caches.
- **`pkg/surface`** — protocol surfaces. Per format, an inbound HTTP handler
  (PEP 503/691, npm registry) paired with an outbound upstream client for
  proxy/cache-through. Depends on `core` only.
- **`cmd/server`** — the only place a concrete Store is built (open a bucket
  from a gocloud URL) and handed to surfaces. cobra + viper; every knob is a
  flag with a matching env var; no config files.
- **`cmd/client`** — a CLI client. Deferred to a later milestone.

See `AGENTS.md` for the path scheme, dot-file rules, the Store verb set, and
the gocloud usage notes.

## Storage model (summary)

```
open-artifact/v1/<scope>/<package>/.tags
open-artifact/v1/<scope>/<package>/<version>/<file>        ← blob
open-artifact/v1/<scope>/<package>/<version>/.meta.<file>  ← per-file API object (digest, etc.)
```

`<scope>` is fixed per proxy at startup (`pypi/global`, `npm/global`).
Leading-dot names are reserved at every level and stripped from listings;
surfaces reject user names that start with `.`. A version "exists" once
anything is written under it — partial publishes are observable, matching
real PyPI/npm.

## Non-goals / deferred (v1)

- **Deletion / yank / unpublish** — semantics differ across formats; separate
  design pass.
- **Registry / namespace (Unity Catalog-style) abstraction** — v1 uses a
  literal scope string per proxy. The Store consumes a string; a future
  `Registry.Scope()` slots in without touching `core` or surfaces.
- **Multi-replica `SetTag`** — v1 guards the `.tags` read-modify-write with an
  in-process mutex; multi-replica needs CAS / conditional writes at the
  bucket layer.
- **SSE-C / per-blob-key encrypted buckets**, cross-region failover, external
  blob paths — revisit when a concrete requirement lands.
- **The client binary** — server first.

## Milestones

1. **core substrate** — nouns, Store interface, Meta, errors (+ unit tests).
2. **blobstore: writes/reads** — paths, `.meta` codec, streaming AddFile /
   ReadFile with digest sidecar (+ integration tests on memblob/fileblob).
3. **blobstore: listings, tags, redirect** — listing verbs with dot-filter,
   `.tags` RMW + mutex, SignedURL + Stat caches (+ concurrency tests).
4. **pypi surface** — PEP 503/691 inbound + upstream client + cache-through
   (+ unit + `pip install` e2e).
5. **npm surface** — registry inbound + upstream + publish + dist-tags
   (+ unit + `npm install` e2e).
6. **server binary** — cobra/viper wiring, bucket-from-URL, mount surfaces
   (+ end-to-end tests through the running server).
7. **client binary** — later.

Quality bar throughout: unit + integration tests are part of each milestone,
not a follow-up. See `AGENTS.md`.
