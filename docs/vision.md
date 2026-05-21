# open-artifact — Vision

## The pitch

A lightweight, stateless, multi-format artifact registry that uses **any
`gocloud.dev/blob` bucket** as its sole storage backend. A small, open-source
alternative to Artifactory or Nexus — for teams who already have an object
store (S3, GCS, Azure Blob) and don't want to run Postgres plus a metadata
service to put a package registry in front of it.

Speak `pip`/`twine`, `npm`, and `mvn` on the front; write plain objects to a
bucket on the back. The bucket's directory tree *is* the index — no database,
no separate search/metadata store, no OCI registry.

## Why a rewrite (and why not OCI)

`yolocs/ocifactory` proved the multi-format-proxy idea but bet on an OCI
registry as its only backend. In practice that was the wrong substrate: OCI's
manifest/blob model fights the "files under a path" shape that package
ecosystems assume, and it ties operators to running (or renting) a registry
just to store tarballs. `gocloud.dev/blob` is the right primitive — a content
store with one interface over S3 / GCS / Azure / filesystem / in-memory.
open-artifact keeps ocifactory's formats, CLI philosophy, and testing bar, and
swaps the backend. **OCI appears here only as historical lineage — never as a
backend requirement.** ocifactory itself is useful only as planning context;
the details needed to build open-artifact live in this repo and its issues.

## The parity target

open-artifact reaches "parity" when it ships, end to end:

- **Three formats:** PyPI (PEP 503/691, twine upload, pip download), npm
  (publish, install, packuments, dist-tags), and Maven (Maven 2 layout,
  deploy, checksums, snapshots).
- **Namespaces:** every package URL is rooted at `/{namespace}/...`. A
  namespace is a control-plane object stored in the bucket, carrying its own
  mode and policy. The legacy idea of a single fixed startup scope (e.g.
  `pypi/global`) is gone — a deployment serves many namespaces.
- **Hosted and proxy modes** per namespace. Hosted accepts uploads; proxy is a
  pull-through cache of an upstream registry with allow/deny/delay filters and
  stale-on-outage fallback.
- **Authn / authz:** OIDC token authentication at the edge; per-namespace
  read/write policy enforced below the protocol surfaces.
- **Observability:** liveness, readiness (with build identity), Prometheus
  metrics, and structured request logs on both planes.
- **Real-client tests:** integration suites that drive `pip`/`twine`, `npm`,
  and `mvn` against an in-process server backed by memblob/fileblob.
- **Release pipeline:** goreleaser, a distroless image, SBOMs, and signatures,
  with operator deployment guides and runbooks.

## Architecture in one breath

```
package manager ──HTTP──▶ surface (pypi|npm|maven) ──▶ namespace-scoped core.Store ──▶ blob.Bucket ──▶ S3/GCS/Azure/fs/mem
                              │   ▲          │              ▲
            OIDC authn + ─────┘   │          └─ proxy ──────┘ (pull-through cache for proxy namespaces)
            per-ns authz          │             upstream client
                                  └─ admin plane: namespace CRUD (separate server)
```

- **`pkg/core`** — the substrate. Chainable noun handles
  (`Package → Version → File`, `Tag → Version`), a `Format` enum, a
  scope-blind `Store` interface, a `Meta` envelope (baseline + opaque
  annotations), and sentinel errors. Knows nothing about HTTP, auth,
  namespaces, upstreams, or metrics.
- **`pkg/core/blobstore`** — `core.Store` over a `*blob.Bucket`. Owns the
  on-bucket path scheme, the `.meta`/`.tags` dot-file conventions, streaming
  uploads with rolling SHA256, in-process tag mutexes, and the
  facade-transparent SignedURL / stat caches.
- **`pkg/namespace`** — namespace model, blob-backed catalog (admin CRUD),
  and the data-plane factory that yields `core.Store` handles scoped to
  `<namespace>/<format>`.
- **`pkg/auth`** — OIDC authenticators, a multi-issuer chain, edge
  middleware, and the per-namespace authorizer enforced below the surfaces.
- **`pkg/proxy`** — shared pull-through primitives: upstream HTTP client,
  blob-backed cache, negative cache, singleflight, and filter chains.
- **`pkg/metrics` / `pkg/logging`** — Prometheus recorder and structured slog
  setup shared by both planes.
- **`pkg/surface`** — protocol surfaces. Per format, an inbound HTTP handler
  paired with an outbound upstream client, plus the shared HTTP/error/redirect
  helpers and real-client test harness. Receives namespace-aware dependencies,
  never a raw store.
- **`cmd/open-artifact`** — the only place a concrete bucket is opened (from a
  gocloud URL), drivers are registered, and the namespace registry, auth,
  metrics, and surfaces are wired. `serve` runs one format; `admin serve` runs
  the control plane. cobra + viper; every knob is a flag with a matching env
  var; no config files.
- **`cmd/artctl`** — the `artctl` admin/inspection CLI. Deferred to post-parity.

See `AGENTS.md` for the full path scheme, dot-file rules, the Store verb set,
namespace/auth/proxy detail, and the gocloud usage notes.

## Storage model (summary)

The namespace is the canonical top-level partition: everything that belongs to
a namespace lives under `open-artifact/v1/<ns>/` (with an optional
`--bucket-prefix` inserted right after the `v1/` root). There is no separate
`data/`, `_control/`, or `_proxy-cache/` tree.

```
open-artifact/v1/<ns>/.meta                                       ← namespace metadata (mode, policy, proxy)
open-artifact/v1/<ns>/<fmt>/.cache/<hash>.body                    ← proxy cache (proxy namespaces)
open-artifact/v1/<ns>/<fmt>/<package>/.tags/<tag>                 ← one object per dist-tag; body = target version
open-artifact/v1/<ns>/<fmt>/<package>/<version>/<file>            ← package blob
open-artifact/v1/<ns>/<fmt>/<package>/<version>/.meta.<file>      ← per-file API object (digest, etc.)
```

A leading `.` is reserved at every level and stripped from listings; surfaces
reject user names that start with `.` (or contain `..`, absolute paths, or
empty segments). The namespace catalog is just the top-level listing — a
namespace "exists" once its `.meta` is written, and a version "exists" once
anything is written under it; partial publishes are observable, matching real
PyPI/npm. Caches live in a `.cache/` folder at the level they apply
(namespace, format, or package) and are opaque to `core.Store`. Dist-tags are
independent objects under `.tags/` whose body is the target version (there is
no shared JSON tags map).

## Non-goals / deferred (v1)

- **Deletion / yank / unpublish** — semantics differ across formats; a
  separate design pass. (Soft-delete *bookkeeping* exists via the package
  index so an empty namespace can be deleted.)
- **Multi-replica `SetTag`** — v1 guards the per-tag read-modify-write with an
  in-process mutex; multi-replica needs CAS / conditional writes at the bucket
  layer.
- **Static username/password auth** — OIDC only in v1.
- **Serving multiple formats from one process** — v1 serves exactly one format
  per `serve` process; run separate processes for separate endpoints.
- **SSE-C / per-blob-key encrypted buckets**, cross-region failover, external
  blob paths — revisit when a concrete requirement lands.
- **The `artctl` client binary** — server first; deferred to post-parity (#29).

## Roadmap

The parity work is tracked as GitHub issues in dependency order. The full
table — including which dependencies block which issue — lives in `AGENTS.md`.
In brief:

1. **#4** Parity 0 — docs, agent rules, roadmap (this work). *Depends #1–#3.*
2. **#5** Parity 1 — runtime foundation: CLI, bucket opener, logging.
3. **#6** Parity 2 — namespace catalog + admin service.
4. **#7** Parity 3 — OIDC authn + per-namespace authz.
5. **#16** Parity 4 — observability: health/ready, metrics, request logs.
6. **#17** Parity 5 — proxy cache primitives + filter policy.
7. **#18** Parity 6 — shared surface framework + real-client harness.
8. **#19 / #20** Parity 7–8 — PyPI hosted, then PyPI proxy.
9. **#21 / #22** Parity 9–10 — npm hosted, then npm proxy.
10. **#23 / #24** Parity 11–12 — Maven hosted, then Maven proxy.
11. **#25** Parity 13 — `serve` command wiring for all three formats.
12. **#26** Parity 14 — CI matrix: real-client, live-upstream, OIDC e2e.
13. **#27** Parity 15 — goreleaser, distroless image, SBOMs, signatures.
14. **#28** Parity 16 — operator docs, deployment guides, runbooks.
15. **#29** Post-parity — the `artctl` client binary.

**Already implemented (#1–#3):** the `core` substrate (nouns, Store, Meta,
errors) and the full `blobstore` Store — streaming writes/reads with digest
sidecars, listing verbs with dot-filtering, per-tag RMW with mutex, and the
SignedURL/stat caches. Everything from #5 onward is future work.

Quality bar throughout: unit + integration tests are part of each issue, not a
follow-up. See `AGENTS.md`.
