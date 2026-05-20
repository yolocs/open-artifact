# AGENTS.md

Guidance for AI coding assistants (and humans) working on **open-artifact**.

## What this project is

open-artifact is a lightweight, stateless, multi-format artifact registry.
It speaks native package-manager protocols (PyPI, npm, …) on the front and
stores everything — blobs, API objects, dist-tags, caches — as objects in a
single **`gocloud.dev/blob` bucket** on the back. No external database, no
bespoke metadata store: the bucket's directory tree *is* the index.

It is the spiritual successor to `yolocs/ocifactory`. ocifactory used an OCI
registry as its sole backend; we concluded that was the wrong substrate and
moved to `gocloud.dev/blob`, which gives us S3 / GCS / Azure / filesystem /
in-memory backends behind one interface. We keep ocifactory's *formats*,
*CLI philosophy*, and *engineering standards*; we replace the storage layer
and re-organize the packages into a `core` / `surface` pair.

## Design pillars (preserve these)

1. **`gocloud.dev/blob` is the sole backend.** A `*blob.Bucket` is the
   storage driver. The `core.Store` wraps it. Do not introduce a second
   storage system or a metadata database.
2. **`core` / `surface` separation.** `pkg/core` is what artifact records
   *are* (data nouns + Store). `pkg/surface` is how clients talk about them
   (per-format HTTP handler + upstream client). Surfaces import `core` only;
   `core` knows nothing about HTTP or upstreams; only `cmd/server`
   constructs a concrete Store and hands it to surfaces.
3. **Production-ready quality.** Every feature ships with unit *and*
   integration tests. Tests are not optional or a follow-up.
4. **Lean operations.** Two binaries (`cmd/server`, later `cmd/client`),
   container-ready, deployable to Cloud Run / Cloudflare-style runtimes.
   Every runtime knob is a CLI flag with a matching env var — no config
   files.
5. **Strong documentation.** Vision doc, per-surface operator notes, and
   this file kept current.

## Code layout

```
cmd/
  server/        ← the server binary (cobra + viper)
  client/        ← the client binary (deferred / later)
pkg/
  core/          ← data nouns, Format enum, Store interface, Meta, errors
    blobstore/   ← core.Store implemented over a gocloud.dev/blob bucket
  surface/       ← Handler interface + shared HTTP/error helpers
    pypi/        ← inbound PEP 503/691 + upstream PyPI client
    npm/         ← inbound npm registry + upstream npm client
internal/version/
docs/
```

### The four nouns

`Package → Version → File`, plus `Tag → Version`. The nouns are **chainable
handles** (interfaces), not value structs. A handle is obtained without I/O;
existence and contents are observed only when a context-taking method is
called. Handles compose downward from a `Store`:

```go
file := store.Package("requests").
        Version("2.31.0").
        File("requests-2.31.0-py3-none-any.whl")
rc, err := file.Read(ctx)
```

Each noun exposes `Namespace()` and no-I/O parent accessors (`Store()`,
`Package()`, `Version()`) plus the read/write verbs for its level.
Creation-time options flow through the variadic `CreateOption`
(`WithAnnotations`); implementations resolve them with
`core.NewCreateConfig`.

### The Store

`Store` is the root handle, **scope-blind at the type level**: the scope (a
path prefix like `pypi/global`) is configured at construction and never
appears as a method argument — it is only readable via `Namespace()`. The
Store hands out `Package` handles (`Package(name)`, `Packages(ctx)`,
`AddPackage(ctx, name, opts...)`); the remaining verbs (list/add versions,
files, tags; resolve/set tag; read file; download URL) live on the noun
handles reachable from it. Sentinel errors (`ErrNotFound`,
`ErrAlreadyExists`, `ErrDigestMismatch`, `ErrUnsupported`) live in
`pkg/core/errors.go` and map to HTTP in a small `surface` helper.
Deletion/yank verbs are out of v1.

### On-bucket path scheme

```
open-artifact/v1/<scope>/<package>/.meta                  ← package API object (optional)
open-artifact/v1/<scope>/<package>/.tags                  ← dist-tags / aliases (optional)
open-artifact/v1/<scope>/<package>/.cache/                ← package-scoped cache (opaque to Store)
open-artifact/v1/<scope>/<package>/<version>/.meta        ← version API object (optional)
open-artifact/v1/<scope>/<package>/<version>/.meta.<file> ← per-file API object (always present; holds digest)
open-artifact/v1/<scope>/<package>/<version>/<file>       ← the file blob
```

Top-level prefix constant is `open-artifact/v1/`. Leading `.` is reserved
at every directory level; listings drop dot-entries when enumerating real
children — **one rule, every level**. The format codec in each surface
**must reject** leading `.` in user-provided package/version/file names.

`.meta` is a baseline envelope (`Digest`, `CreatedAt`, `UpdatedAt`) plus an
opaque caller-owned `Annotations map[string]any` the Store round-trips but
never interprets. `size` is intentionally absent — derive from bucket
attributes.

### gocloud.dev/blob notes

- Open buckets from a URL (`blob.OpenBucket(ctx, "s3://…")`,
  `mem://`, `file:///…`) so the backend is a deployment flag.
- Streaming upload: `bucket.NewWriter` + rolling SHA256 on the write path;
  write the `.meta.<file>` sidecar after the writer closes successfully.
- `File.DownloadURL` wraps `bucket.SignedURL` behind a mandatory,
  facade-transparent LRU + singleflight cache with per-cloud TTL parsing.
  memblob/fileblob return no signed URL → cache the miss and return an empty
  URL so the surface falls back to streaming `File.Read`.
- `bucket.List` with a delimiter gives the directory children for the
  listing verbs; sort caller-side if order matters.

## Testing standards (non-negotiable)

- **Unit + integration for every feature.** Integration tests live behind a
  `//go:build integration` tag and run the real `core.Store` against the
  **memblob** (and where relevant **fileblob** via `t.TempDir()`) backends —
  no separate fake Store implementation. Cover the hard cases: concurrent
  `AddFile` to one version, AddFile/ReadFile races, `SetTag` races,
  empty-scope listings, leading-dot rejection at the codec, digest mismatch.
- End-to-end surface tests exercise a real client where feasible (`pip
  install`, `npm install`) against an in-process server.
- `t.Parallel()` and `t.Context()` in every test.
- Table-driven tests for multiple inputs.
- Compare with `cmp.Diff`, not field-by-field assertions.
- **Fakes, not mocks.** Exercise real layers together.

## Adding a new surface (checklist)

1. New `pkg/surface/<format>` package: inbound handler + upstream client.
2. Scope-prefixed mount; flags/env for the upstream URL and scope.
3. Format codec rejects leading-dot names; maps `core` sentinels to the
   format's HTTP error shape via the shared helper.
4. Unit tests for the codec + handler; integration tests against memblob;
   a real-client end-to-end test.
5. Operator notes in `docs/`.

## Conventions

- Go only; follow Effective Go.
- cobra + viper for the CLI; every flag has a matching env var; no config
  files.
- Build/test via the `Makefile`; releases via goreleaser.
- **Always sign off commits** (`git commit -s`) — the repo enforces DCO.
- Keep `core` free of `surface` and backend imports; keep the dependency
  arrows pointing one way.
