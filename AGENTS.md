# AGENTS.md

Authoritative implementation guide for AI coding assistants (and humans)
working on **open-artifact**. This file plus the open GitHub issues are
self-contained: you should be able to build open-artifact from this repo
alone, without opening any predecessor project.

> **Predecessor code is historical context only.** open-artifact is the
> spiritual successor to `yolocs/ocifactory`. You may study ocifactory while
> *planning* — to understand a format's wire protocol or recall a design
> trade-off — but every detail needed to *implement* open-artifact must live
> in this repo: in this file, in `docs/`, and in the parity issues. Do not
> cite ocifactory as a source of truth, copy its storage model, or assume its
> OCI backend. If something is missing here, add it here.

## What this project is

open-artifact is a lightweight, stateless, multi-format artifact registry. It
speaks native package-manager protocols — **PyPI, npm, and Maven** — on the
front and stores everything (blobs, API objects, dist-tags, namespace
metadata, proxy caches) as plain objects in a single **`gocloud.dev/blob`
bucket** on the back. No external database, no bespoke metadata store: the
bucket's directory tree *is* the index.

It is a small, open-source alternative to Artifactory or Nexus for teams who
already run an object store (S3 / GCS / Azure Blob / filesystem) and don't
want to stand up a database to put a package registry in front of it.

Each registry endpoint is partitioned into **namespaces**. Every
package-manager URL is rooted at `/{namespace}/...`. A namespace is a
control-plane object — stored in the same bucket — that carries its own
**mode** and **policy**:

- **Hosted** namespaces accept uploads from clients and serve what was
  published.
- **Proxy** namespaces are pull-through caches of an upstream registry
  (PyPI, npmjs.org, Maven Central), populating the bucket on cold reads and
  serving from it thereafter.

Clients authenticate with **OIDC** tokens; each namespace's **policy**
decides whether the authenticated subject may read or write. Both planes
(data and admin) expose liveness, readiness, and Prometheus metrics.

## Lineage (and why not OCI)

`yolocs/ocifactory` proved the multi-format-proxy idea but bet on an OCI
registry as its only backend. OCI's manifest/blob model fights the
"files under a path" shape that package ecosystems assume and ties operators
to running a registry just to store tarballs. `gocloud.dev/blob` is the right
primitive: a content store with one interface over S3 / GCS / Azure /
filesystem / in-memory. open-artifact keeps ocifactory's *formats*, *CLI
philosophy*, and *engineering bar*; it replaces the storage layer and
re-organizes the packages. **OCI is lineage, not a requirement** — it must
not appear anywhere in this repo as a backend dependency.

## Design pillars (preserve these)

1. **`gocloud.dev/blob` is the sole durable backend.** A `*blob.Bucket` is
   the storage driver; `core.Store` wraps it. Do not introduce a second
   storage system or a metadata database. In-memory caches (signed-URL,
   stat, negative, policy) are allowed but must be reconstructible from the
   bucket.
2. **`core` is pure.** `pkg/core` knows nothing about HTTP, auth,
   namespaces, upstream registries, or metrics. It is what artifact records
   *are*. Everything else composes around it; the dependency arrows point
   one way (see "Architecture").
3. **Production-ready quality.** Every feature ships with unit *and*
   integration tests in the same change. Tests are not a follow-up.
4. **Lean operations.** One binary (`open-artifact`) with `serve` and
   `admin serve` subcommands, container-ready, deployable to Cloud Run /
   Cloudflare-style runtimes. Every runtime knob is a CLI flag with a
   matching env var — no config files.
5. **Strong documentation.** Vision doc, per-surface operator notes, and
   this file kept current with behavior changes.

## Architecture and the dependency rule

```
package manager ──HTTP──▶ surface (pypi|npm|maven) ──▶ namespace-scoped core.Store ──▶ blob.Bucket
                              │   ▲          │              ▲
            OIDC authn + ─────┘   │          └─ proxy ──────┘ (pull-through cache for proxy namespaces)
            per-ns authz          │             upstream client
                                  └─ admin plane: namespace CRUD (separate server)
```

The arrows point one way. Enforce it in code review:

- `pkg/core` imports nothing from `surface`, `namespace`, `auth`, `proxy`,
  `metrics`, or backend driver packages.
- `pkg/surface/*` receive **namespace-aware** dependencies (a namespace
  registry/factory that yields scoped, authorized `core.Store` handles) —
  never a raw `*blobstore.Store`. This makes it impossible to bypass authz
  or namespace scoping by accident.
- Only the command layer (`cmd/...`) opens a concrete bucket, registers Go
  CDK drivers, builds the namespace registry, auth middleware, metrics, and
  hands them to surfaces.

## Code layout

Implemented today is in **plain text**; planned packages introduced by the
parity issues are marked *(planned, #N)*.

```
cmd/
  open-artifact/     ← single binary: `serve` + `admin serve` (planned, #5; today scaffolded under cmd/server)
  artctl/            ← admin/inspection CLI (deferred, post-parity #29)
pkg/
  core/              ← data nouns, Format enum, Store interface, Meta, sentinel errors
    blobstore/       ← core.Store implemented over a gocloud.dev/blob bucket
  logging/           ← slog setup, context helpers, stable fields (planned, #5)
  namespace/         ← Namespace/Spec model, blob-backed Store, data-plane registry/factory (planned, #6)
  auth/              ← Authenticator/Authorizer, middleware, sentinels (planned, #7)
    oidc/            ← OIDC discovery + token verification (planned, #7)
    chain/           ← multi-issuer authenticator chain (planned, #7)
  metrics/           ← Recorder interface, NoOp + Prometheus impls (planned, #16)
  proxy/             ← upstream HTTP client, blob-backed cache, negative cache, filters (planned, #17)
  surface/           ← Handler interface + shared HTTP/error/redirect helpers + test harness (planned framework, #18)
    admin/           ← namespace CRUD HTTP API (planned, #6)
    pypi/            ← PEP 503/691 hosted + proxy (planned, #19/#20)
    npm/             ← npm registry hosted + proxy (planned, #21/#22)
    maven/           ← Maven 2 layout hosted + proxy (planned, #23/#24)
internal/version/    ← single source of build identity
docs/
```

Final package names are whatever the owning issue specifies; the issue is the
contract. Keep `core` free of every concern listed above it.

## The four nouns (implemented)

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
`core.NewCreateConfig`. A version "exists" once anything is written under it —
partial publishes are observable, matching real PyPI/npm.

## The Store (implemented)

`Store` is the root handle, **scope-blind at the type level**: the scope (a
path prefix) is configured at construction and never appears as a method
argument — it is only readable via `Namespace()`. The Store hands out
`Package` handles (`Package(name)`, `Packages(ctx)`,
`AddPackage(ctx, name, opts...)`); the remaining verbs (list/add versions,
files, tags; resolve/set tag; read file; download URL) live on the noun
handles reachable from it.

The scope string is *not* a literal like `pypi/global`. In the parity design
the data-plane factory (#6) constructs a `blobstore.Store` bound to scope
**`<namespace>/<format>`**, so packages for namespace `team-a`'s PyPI endpoint
live under `open-artifact/v1/team-a/pypi/...`. `core` neither knows nor cares
how the scope is computed.

Sentinel errors (`ErrNotFound`, `ErrAlreadyExists`, `ErrDigestMismatch`,
`ErrUnsupported`) live in `pkg/core/errors.go` and map to HTTP in a shared
`surface` helper. Deletion/yank verbs are out of v1.

## On-bucket path scheme

Top-level prefix constant is **`open-artifact/v1/`**. When `--bucket-prefix`
is set it is inserted right after the root: `open-artifact/v1/<prefix>/...`.

**The namespace is the canonical top-level partition.** Everything that
belongs to a namespace lives under `open-artifact/v1/<ns>/` — there is no
separate `data/`, `_control/`, or `_proxy-cache/` tree. A namespace's own
metadata is the `.meta` object directly under it; package data is grouped by
format below that:

```
open-artifact/v1/<ns>/.meta                                  ← namespace metadata (mode, policy, proxy spec)
open-artifact/v1/<ns>/.cache/...                             ← namespace-level cache (opaque to Store)
open-artifact/v1/<ns>/<fmt>/.cache/...                       ← format-level cache, e.g. proxy pull-through
open-artifact/v1/<ns>/<fmt>/<package>/.meta                  ← package API object (optional)
open-artifact/v1/<ns>/<fmt>/<package>/.tags/<tag>            ← one object per dist-tag; body = target version
open-artifact/v1/<ns>/<fmt>/<package>/.cache/...             ← package-scoped cache (opaque to Store)
open-artifact/v1/<ns>/<fmt>/<package>/<version>/.meta        ← version API object (optional)
open-artifact/v1/<ns>/<fmt>/<package>/<version>/.meta.<file> ← per-file API object (always present; holds digest)
open-artifact/v1/<ns>/<fmt>/<package>/<version>/<file>       ← the file blob
```

The data-plane factory (#6) binds a `blobstore.Store` to scope `<ns>/<fmt>`;
the blobstore path helpers lay out everything from `<package>` down. The
namespace catalog (admin plane, #6) owns the `<ns>/.meta` object.

**No side indexes — listing is the index.** The namespace catalog is the
top-level child listing under the root (drop dot-entries); a namespace
*exists* once its `<ns>/.meta` is written, mirroring how a version exists once
anything is written under it. Delete-emptiness is "no non-dot children under
`<ns>/`". There are no `_control`/`namespace-index`/`package-index` sentinels.

**Caches live in `.cache/`, at the level they apply** — namespace,
format (proxy pull-through cache for proxy namespaces), or package. A proxy
namespace caches upstream metadata+body under `<ns>/<fmt>/.cache/...`
(e.g. `<sha256(key)>.body` + `<sha256(key)>.json`). Everything under a
`.cache/` is opaque to `core.Store` and never appears in package/version/file
listings.

**Reserved-name discipline — one rule, every level.** A leading `.` is
reserved at every directory level; listings drop dot-entries when enumerating
real children (namespaces, formats, packages, versions, files). Because
namespace names may not begin with `.` or `_` (see name validation below) and
formats are a fixed allow-list, namespace/format directories never collide
with the dot-prefixed metadata/cache objects. The format codec in each surface
**must reject** leading `.`, `..`, absolute paths, and empty path segments in
user-provided package/version/file/tag names — so user data is never silently
hidden.

`.meta` is a baseline envelope (`Digest`, `CreatedAt`, `UpdatedAt`) plus an
opaque caller-owned `Annotations map[string]any` the Store round-trips but
never interprets. `size` is intentionally absent — derive it from bucket
attributes.

## Namespaces and modes (planned, #6/#7)

- **Name validation:** 1–64 chars, lowercase ASCII letters/digits/`-`, no
  leading/trailing `-`, no leading `_`/`.`; reject reserved names (`admin`,
  `healthz`, `readyz`, `metrics`, `simple`, `maven2`, `v2`, `npm`, `pypi`,
  `_control`, `_proxy-cache`, `open-artifact`).
- **Spec** carries `schema_version` (current = 1), `mode` (empty/`hosted` or
  `proxy`), `policy` (readers/writers subject matchers), `proxy`
  (upstream + filters), and an opaque `format` map that must round-trip
  unknown JSON. Hosted is the default; `proxy` requires an absolute http(s)
  `upstream`; hosted must reject a non-empty proxy block.
- **Admin API** (`/admin/v1/namespaces/...`, #6) is the only writer of
  namespace metadata. It has **no built-in auth** — it must log a startup
  warning and operators must deploy it behind network/platform controls.
- **Authz** is enforced *below* surface protocol code, in the
  namespace-scoped Store wrapper: every read/write op is authorized against
  the namespace policy before it reaches `core.Store`. Empty policy is
  deny-all; readers and writers are independent. A compiled-policy cache
  (default 60s, singleflight, negative caching) is invalidated immediately on
  admin `Put`/`Delete`.

## Auth (planned, #7)

OIDC only in v1. Credentials arrive as `Authorization: Bearer <token>` or
`Authorization: Basic base64("<sentinel-user>:<token>")` (sentinel users:
`_oidc`, `oauth2accesstoken`, `_token`). Middleware returns 401 with both
`Bearer` and `Basic` `WWW-Authenticate` challenges on missing/invalid
credentials, and never wraps `/healthz`, `/readyz`, or `/metrics`. A
multi-issuer chain tries authenticators in order: `ErrNoCredential` falls
through, the first success wins, the first hard error stops. `--disable-authn`
swaps in an always-anonymous authenticator and logs a warning.

## Observability (planned, #16)

A wrapper intercepts `/healthz`, `/readyz`, and the metrics path before auth
or format routing. Readiness probes the backend (data plane: bucket
reachable; admin: namespaces listable) with a 2s timeout and a 1s success
cache, and reports build identity. Metrics use the `open_artifact_` prefix
via a `metrics.Recorder` (`NoOp()` / `NewPrometheus(reg)`); blob backend calls
and redirect outcomes are instrumented without changing the `core.Store` API.
Structured request logs carry stable fields and never log `Authorization`.

## Proxy primitives (planned, #17)

Shared, format-agnostic pull-through machinery: a context-aware upstream HTTP
client with body caps, a blob-backed mutable metadata+body cache under the
format-level `<ns>/<fmt>/.cache/`, an in-memory negative cache for upstream
404s, process-local
singleflight for cold fills, and an ordered allow/deny/delay filter chain
validated as part of namespace spec validation. Cold-miss bytes flow through
open-artifact (never redirect clients to public upstream URLs); cache hits may
still use backend signed-URL redirects because those target the
operator-controlled bucket. Reader policy is sufficient to populate cache.

## gocloud.dev/blob notes

- Open buckets from a URL (`blob.OpenBucket(ctx, "s3://…")`, `mem://`,
  `file:///…`) so the backend is a deployment flag. Register drivers
  (`memblob`, `fileblob`, `s3blob`, `gcsblob`, `azureblob`) in the command
  layer only — never in `pkg/core`.
- The command owns bucket lifecycle: open once at startup, close on shutdown.
  `blobstore.Store` must not close a caller-owned bucket.
- Streaming upload: `bucket.NewWriter` + rolling SHA256 on the write path;
  write the `.meta.<file>` sidecar after the writer closes successfully.
- `File.DownloadURL` wraps `bucket.SignedURL` behind a mandatory,
  facade-transparent LRU + singleflight cache with per-cloud TTL parsing.
  memblob/fileblob return no signed URL → cache the miss and return an empty
  URL (nil error) so the surface falls back to streaming `File.Read`.
- `bucket.List` with a delimiter gives the directory children for the listing
  verbs; sort caller-side if order matters.

## CLI conventions

- Single binary `open-artifact`; cobra for commands, viper for flag/env
  resolution. Subcommands: `serve` (data plane, default port 8080) and
  `admin serve` (control plane, default port 8081). `--version` prints
  version, commit, and `GOOS/GOARCH`.
- **Every flag has a matching env var.** Prefix `OPEN_ARTIFACT`, dashes →
  underscores (e.g. `--bucket-url` ↔ `OPEN_ARTIFACT_BUCKET_URL`). Also bind
  platform `PORT` → `--port`. No config files.
- Root command uses `SilenceUsage`/`SilenceErrors` so errors are testable.
  Validate config at startup and fail with clear, joined errors.

## Testing standards (non-negotiable)

- **Unit + integration for every feature**, in the same change. Integration
  tests live behind a `//go:build integration` tag and run the real layers
  against **`mem://`** (and where relevant **`file://`** via `t.TempDir()`)
  buckets — **no mock `Store`** for storage or surface behavior. Use fakes,
  not mocks; exercise real layers together.
- `t.Parallel()` and `t.Context()` in every test, unless a documented
  process-global exception applies (e.g. env-var or registry mutation).
- Table-driven tests for multiple inputs; compare with `cmp.Diff`, not
  field-by-field assertions.
- Cover the hard cases: concurrent `AddFile` to one version, AddFile/ReadFile
  races, `SetTag` races, empty-scope listings, leading-dot/`..` rejection at
  the codec, digest mismatch, cross-namespace isolation, deny-all policy,
  proxy stale-fallback, and filter ordering.
- End-to-end surface tests drive a real client (`pip`/`twine`, `npm`, `mvn`)
  against an in-process server via the shared harness (#18). Do **not**
  require Docker — the backend is memblob/fileblob; only the package-manager
  client tools need to be installed.
- Every change: `go test -race ./...`; for storage/surface work also
  `go test -race -tags=integration ./...`.

## Adding a new surface (checklist)

1. New `pkg/surface/<format>` package: inbound protocol handler + outbound
   upstream client. Construct it from the **namespace registry** (#6/#7),
   never a raw `*blobstore.Store`.
2. Support both modes: hosted (accept uploads, immutable writes unless
   `--allow-overwrite`) and proxy (writes disabled, pull-through via the #17
   primitives). Read `Spec.Mode` per request so admin mode switches take
   effect without restart.
3. Format codec rejects leading-dot/`..`/absolute/empty-segment names and
   internal-prefix collisions; maps `core`/`namespace`/`auth` sentinels to
   the format's HTTP error shape via the shared `surface` helpers.
4. Use the shared helpers: JSON/error writers, `RedirectOrStreamFile`, HEAD
   handling, `MaxBytesReader`, metrics op labeling.
5. Unit tests for the codec + handler; integration tests against `mem://`;
   a real-client end-to-end test through the harness.
6. Operator notes in `docs/`; flags/env for upstream URL, upload caps, and
   cache TTLs.

## Conventions

- Go only unless there is a strong reason. Follow Effective Go.
- **Explicit, boring code over clever abstractions.**
- Build/test via the `Makefile`; releases via goreleaser.
- Open a tracking issue for non-trivial work; update docs in the same change
  as the behavior they describe.
- **Always sign off commits** (`git commit -s`) — the repo enforces DCO.

## Roadmap (parity issues, in dependency order)

Done is what shipped before this guide; everything else is open. Each issue is
self-contained — read it before starting.

**Shipped (core substrate):**

- **#1** core: nouns, Format enum, Store interface, Meta, sentinel errors.
- **#2** blobstore: streaming `AddFile`/`ReadFile` over a `blob.Bucket`.
- **#3** blobstore: listings, tags, cached blob-redirect — `core.Store`
  complete.

**Open (parity), with dependencies:**

| Issue | Title | Depends on |
|------|-------|------------|
| **#4** (this) | Parity 0: refresh agent rules, docs, roadmap | #1, #2, #3 |
| **#5** | Parity 1: runtime foundation, CLI, bucket opener, logging | #4 |
| **#6** | Parity 2: namespace catalog + admin service | #5 |
| **#7** | Parity 3: OIDC authn, middleware, per-namespace authz | #6 |
| **#16** | Parity 4: observability endpoints, Prometheus metrics, request logs | #5, #6 |
| **#17** | Parity 5: proxy cache primitives + filter policy | #6, #16 |
| **#18** | Parity 6: shared surface framework + real-client test harness | #7, #16 |
| **#19** | Parity 7: PyPI hosted surface (PEP 503/691, twine, pip) | #18 |
| **#20** | Parity 8: PyPI proxy mode (pull-through + stale fallback) | #19, #17 |
| **#21** | Parity 9: npm hosted surface (publish, install, dist-tags) | #18 |
| **#22** | Parity 10: npm proxy mode | #21, #17 |
| **#23** | Parity 11: Maven hosted surface (Maven 2 layout, snapshots) | #18 |
| **#24** | Parity 12: Maven proxy mode | #23, #17 |
| **#25** | Parity 13: `serve` command wiring for PyPI/npm/Maven | #7, #16, #19–#24 |
| **#26** | Parity 14: CI matrix, real-client + live-upstream + OIDC e2e | #25 |
| **#27** | Parity 15: goreleaser, distroless image, SBOMs, signatures | #25 |
| **#28** | Parity 16: operator docs, deployment guides, runbooks | #25, #26, #27 |
| **#29** | Post-parity: `artctl` client binary for admin/inspection | #25 |
