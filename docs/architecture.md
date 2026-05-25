# open-artifact — Architecture

The design reference for open-artifact: what the pieces are, how they fit, and
the invariants that hold them together. This is a **living document** — keep it
current as the design evolves, and update it in the same change as the behavior
it describes. For how we work on the project see [`../AGENTS.md`](../AGENTS.md);
for how to run and configure the binary see
[`operations.md`](operations.md); the roadmap lives in GitHub issues.

This describes the target architecture. The `core` substrate with its
`blobstore` implementation, the runtime foundation (CLI, bucket opener,
logging, server lifecycle), namespaces, auth, observability, PyPI hosted and
proxy serving, the npm hosted surface, and the shared proxy primitives exist
today; the npm proxy and the Maven surfaces are described here as the design
they are being built toward. Where it matters, sections note what is
implemented versus planned.

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

Clients authenticate with **OIDC** tokens; each namespace's **policy** decides
whether the authenticated subject may read or write. Both planes (data and
admin) expose liveness, readiness, and Prometheus metrics.

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

## Invariants

These hold across the whole system; everything below is built to preserve them.

1. **`gocloud.dev/blob` is the sole durable backend.** A `*blob.Bucket` is the
   storage driver; `core.Store` wraps it. No second storage system, no
   metadata database. In-memory caches (signed-URL, stat, negative, policy)
   are allowed but must be reconstructible from the bucket.
2. **`core` is pure.** `pkg/core` knows nothing about HTTP, auth, namespaces,
   upstream registries, or metrics. It is what artifact records *are*;
   everything else composes around it, and the dependency arrows point one
   way.
3. **The namespace is the canonical partition.** Everything that belongs to a
   namespace lives under its subtree in the bucket; the bucket layout, scoping,
   and authz all key off it.
4. **One product, three formats.** PyPI, npm, and Maven share one framework and
   behave identically except where the wire protocol genuinely differs.

## Dependency rule

```
package manager ──HTTP──▶ surface (pypi|npm|maven) ──▶ namespace-scoped core.Store ──▶ blob.Bucket
                              │   ▲          │              ▲
            OIDC authn + ─────┘   │          └─ proxy ──────┘ (pull-through cache for proxy namespaces)
            per-ns authz          │             upstream client
                                  └─ admin plane: namespace CRUD (separate server)
```

The arrows point one way. Enforce it in review:

- `pkg/core` imports nothing from `surface`, `namespace`, `auth`, `proxy`,
  `metrics`, or backend driver packages.
- `pkg/surface/*` receive **namespace-aware** dependencies (a namespace
  registry/factory that yields scoped, authorized `core.Store` handles) —
  never a raw `*blobstore.Store`. This makes it impossible to bypass authz or
  namespace scoping by accident.
- Only the command layer (`cmd/...`) opens a concrete bucket, registers Go CDK
  drivers, builds the namespace registry, auth middleware, metrics, and hands
  them to surfaces.

## Code layout

Implemented today is in **plain text**; planned packages are marked
*(planned)*. Final package names are decided by the work that introduces them.

```
cmd/
  open-artifact/     ← single binary: `serve` + `admin serve`
  artctl/            ← admin/inspection CLI (deferred to post-parity)
internal/
  version/           ← single source of build identity
pkg/
  command/           ← cobra/viper command tree, config resolution + validation
  admin/             ← namespace CRUD HTTP API (format-agnostic control plane)
  bucket/            ← command-layer bucket opener; registers Go CDK blob drivers
  serving/           ← shared HTTP server lifecycle (graceful shutdown) + logger middleware
  core/              ← data nouns, Format enum, Store/Cache interfaces, Meta, sentinel errors
    blobstore/       ← core.Store + core.Cache implemented over a gocloud.dev/blob bucket
  logging/           ← slog setup, context helpers, stable fields
  namespace/         ← Namespace/Spec model, blob-backed Store, data-plane registry/factory
  auth/              ← Authenticator/Authorizer, middleware, sentinels
    oidc/            ← OIDC discovery + token verification
    chain/           ← multi-issuer authenticator chain
  metrics/           ← Recorder interface, NoOp + Prometheus impls
  observability/     ← liveness/readiness/metrics wrapper, request metrics + logging, format/op labeling
  proxy/             ← shared pull-through primitives (format-agnostic)
    httpclient/      ← context-aware upstream GET/HEAD with body caps + status mapping
    negcache/        ← in-memory negative cache for upstream 404s
    singleflight/    ← typed cold-fill coalescer
    filter/          ← allow/deny/delay config schema, validation, and decision engine
  surface/           ← Handler interface + shared HTTP/error/redirect helpers + test harness (planned framework)
    pypi/            ← PEP 503/691 hosted + pull-through proxy
    npm/             ← npm registry hosted; pull-through proxy (planned)
    maven/           ← Maven 2 layout hosted + proxy (planned)
docs/
```

Keep `core` free of every concern listed above it.

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

The scope string is *not* a literal like `pypi/global`. The data-plane factory
constructs a `blobstore.Store` bound to scope **`<namespace>/<format>`**, so
packages for namespace `team-a`'s PyPI endpoint live under
`open-artifact/v1/team-a/pypi/...`. `core` neither knows nor cares how the
scope is computed.

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

The data-plane factory binds a `blobstore.Store` to scope `<ns>/<fmt>`; the
blobstore path helpers lay out everything from `<package>` down. The namespace
catalog (admin plane) owns the `<ns>/.meta` object and is its only writer.

**`blobstore` owns name encoding and validation; callers pass raw names.** Every
user-provided name component — package, version, file, tag, cache key — is
rendered into a single path-safe bucket segment by one helper
(`encodeSegment`/`decodeSegment`, built on `url.QueryEscape`) and round-trips
losslessly through listing. `QueryEscape` escapes aggressively — every reserved
or non-alphanumeric byte except `-_.~` — so the segment stays broadly
blob-backend compatible (npm scoped names like `@scope/name` →
`%40scope%2Fname` stay one bucket child rather than nesting; `:` → `%3A` etc.).
A name that is empty or **begins with `.`** is **rejected** (`ErrInvalidName`),
not escaped: a leading dot is reserved for Store-owned objects
(`.meta`/`.tags`/`.cache`), and there is no legitimate reason to accept such
input, so the Store refuses it rather than smuggling it through. A valid name
never QueryEscapes to a leading `.`, so an encoded segment can never collide
with a reserved dot-file. Validation runs on both read and write paths (a no-I/O
handle built from a bad name carries the error and surfaces it on first use), so
a surface does **not** sanitize names — it forwards whatever a client sends and
the Store keeps it safe, lossless, or rejected.

**No side indexes — listing is the index.** The namespace catalog is the
top-level child listing under the root (drop dot-entries); a namespace
*exists* once its `<ns>/.meta` is written, mirroring how a version exists once
anything is written under it. Delete-emptiness is "no non-dot children under
`<ns>/`". There are no `_control`/`namespace-index`/`package-index` sentinels.

**Caches are Files in `.cache/`, at the level they apply**, and follow the File
verb pattern: each level has `AddCache(ctx, key, body)` (write, mirroring
`AddFile`) and `Cache(key)` (a no-I/O handle, mirroring `File`) on `Store`
(format level), `Package`, and `Version`. The handle is a `core.CacheFile` —
the same blob+`.meta` sidecar storage as a regular File, reusing the same
read/digest/write code, differing only in living under a reserved `.cache/`
folder, being mutable+evictable (`AddCache` overwrites; the handle has
`Delete`), and having no Package/Version parent. A proxy namespace caches
upstream index/metadata (a PyPI simple page, an npm packument) keyed by a
logical name (e.g. `simple:requests`); artifact bytes are **not** cached — they
become real Files via `AddFile`. Cache files never appear in
`Packages`/`Versions`/`Files` listings (the `.cache/` segment is a dropped
dot-entry); writing a package/version-level cache does, like any object,
materialize that package/version directory. Cache fill authorizes as a **read**,
so reader policy suffices.

**Reserved-name discipline — one rule, every level.** A leading `.` is
reserved at every directory level; listings drop dot-entries when enumerating
real children (namespaces, formats, packages, versions, files). Namespace names
may not begin with `.` or `_` and formats are a fixed allow-list, so
namespace/format directories never collide with dot-prefixed metadata/cache
objects. At the package/version/file/tag level, `blobstore` **rejects** a
user name that begins with `.` (`ErrInvalidName`), so user data can never be
silently hidden or collide with `.meta`/`.tags`/`.cache` — the Store guarantees
this rather than relying on each surface codec to reject names.

`.meta` is a baseline envelope (`Digest`, `Size`, `CreatedAt`, `UpdatedAt`)
plus an opaque caller-owned `Annotations map[string]any` the Store round-trips
but never interprets. `Size` is the blob's byte length, counted on the write
path (for free, alongside the rolling digest) and trusted the same way the
digest is — so one `.meta` read yields digest, size, and annotations without a
separate bucket-attributes call. When the sidecar is absent the Store
recomputes both digest and size from the bucket's object attributes.

## Namespaces and modes

- **Name validation:** 1–64 chars, lowercase ASCII letters/digits/`-`, no
  leading/trailing `-`, no leading `_`/`.`; reject reserved names (`admin`,
  `healthz`, `readyz`, `metrics`, `simple`, `maven2`, `v2`, `npm`, `pypi`,
  `_control`, `_proxy-cache`, `open-artifact`).
- **Spec** carries `schema_version` (current = 1), `mode` (empty/`hosted` or
  `proxy`), `policy` (readers/writers subject matchers), `proxy`
  (upstream + filters), and an opaque `format` map that must round-trip
  unknown JSON. Hosted is the default; `proxy` requires an absolute http(s)
  `upstream`; hosted must reject a non-empty proxy block. On write the spec is
  normalized — `schema_version` is stamped to the current version and an
  explicit `mode: "hosted"` is collapsed to empty so hosted JSON stays compact;
  a stored spec newer than this binary understands is rejected on read.
- **Store** (`pkg/namespace`) is the blob-backed catalog: `Put`/`Get`/`List`/
  `Delete` over `<ns>/.meta`, with an `OnChange` mutation hook for cache/authz
  invalidation. Listing is the index — `List` enumerates top-level child
  directories (dropping dot-entries) and loads each `.meta`, skipping a
  directory that has none. `Delete` refuses a namespace that still holds
  package data (a non-dot child under any non-dot format directory); a
  `.meta`/`.cache`-only namespace is empty.
- **Registry** (`pkg/namespace`) is the data-plane factory: `For(ns, fmt)`
  validates the namespace name and format (`pypi`, `npm`, `maven`) and yields a
  `Scoped` handle whose `Store()` is a `blobstore.Store` bound to scope
  `<ns>/<fmt>` and whose `Spec(ctx)` resolves the namespace spec live — admin
  changes are visible without restart, and an unknown namespace maps to
  `ErrNotFound`.
- **Admin API** (`/admin/v1/namespaces/...`, `pkg/admin`) is the only
  writer of namespace metadata: `PUT` (201 create / 200 update), `GET`,
  `DELETE` (204; 409 when non-empty), and `GET` list. Errors use
  `{"error":"…"}` and map invalid name/spec/schema → 400, missing → 404,
  non-empty delete → 409. It has **no built-in auth** — it logs a startup
  warning and operators must deploy it behind network/platform controls.
- **Authz** is enforced *below* surface protocol code, in the
  namespace-scoped Store wrapper: every read/write op is authorized against
  the namespace policy before it reaches `core.Store`. Empty policy is
  deny-all; readers and writers are independent. A compiled-policy cache
  (default 60s, singleflight, negative caching) is invalidated immediately on
  admin `Put`/`Delete`.

## Auth

OIDC only in v1 (`pkg/auth`). Credentials arrive as `Authorization: Bearer
<token>` or `Authorization: Basic base64("<sentinel-user>:<token>")` (sentinel
users: `_oidc`, `oauth2accesstoken`, `_token`); a Basic header with any other
username is not a password login and yields `ErrNoCredential`. An
`Authenticator` turns a request into an `AuthContext` (`Issuer`, `ID`, `Email`,
`Claims`, `Kind`); the sentinel errors `ErrNoCredential`, `ErrInvalidToken`,
`ErrUnauthorized`, and `ErrUnknownOp` are all `errors.Is`-matchable.

`auth.Middleware` returns 401 with both `Bearer` and `Basic`
`WWW-Authenticate` challenges on missing/invalid credentials, and never wraps
`/healthz`, `/readyz`, or `/metrics` (#25 installs it only inside format
routes). The OIDC authenticator (`pkg/auth/oidc`) does lazy discovery + JWKS
(both size-capped), peeks the unverified `iss` before any network work so a
token for another issuer falls through as `ErrNoCredential`, verifies audience
exactly, accepts only the issuer's advertised signing algorithms (never
`none`), and reads `email` only when `email_verified` is true. A multi-issuer
chain (`pkg/auth/chain`) tries authenticators in order: `ErrNoCredential` falls
through, the first success wins, the first hard error stops. `--disable-authn`
swaps in `AlwaysAnonymous` (issuer/id/kind = `anonymous`) and logs a warning;
otherwise the data plane refuses to start without OIDC issuers and an audience.

### Per-namespace authorization

A namespace `Policy` lists `readers` and `writers` as `SubjectMatcher`s
(`issuer`, `sub_match`, `email`, `claims_match`, `kind`). Matching: `issuer`
and `email` compare for equality; `sub_match` and every `claims_match` value
are RE2 regexes anchored at both ends (non-string claims are JSON-encoded with
stable key order first); an empty `kind` means `oidc` (`basictoken` is reserved
and rejected, unknown kinds rejected). Within a matcher all populated fields
are ANDed; across the selected list any matcher allows. Readers and writers are
independent (write does not imply read), an empty policy is deny-all, a nil
`AuthContext` is unauthorized, and an unknown op is `ErrUnknownOp`. Policy
validation happens at admin write time and maps to 400.

Enforcement lives **below** the surface protocol code and **inside** the
storage handle rather than in a decorator: `blobstore.Store` exposes a generic,
auth-agnostic `Guard` hook (`func(ctx, write bool) error`) that it consults at
the start of every read/write before touching the bucket. `pkg/core` therefore
still imports nothing from `auth` — the namespace layer supplies a `Guard` that
closes over the compiled policy and the request's subject. `Registry.Authorized`
resolves the namespace, binds that guard, and returns a namespace-and-format
scoped `core.Store`; the guard maps reads to `OpRead`
(existence/listing/read/download-url/tag-ref) and writes to `OpWrite`
(add/annotate/set-tag). Because the check is in the store itself, there is no
wrapper handle to navigate around. An unknown namespace maps to namespace
`ErrNotFound` (404); a denied op maps to `auth.ErrUnauthorized` (403). The
compiled policy is served from a per-namespace cache (default 60s, including a
negative entry for missing namespaces) that collapses concurrent misses with
singleflight and is invalidated immediately by the catalog's `OnChange` hook on
admin `Put`/`Delete` within a process; across processes the TTL bounds
staleness. `WithPolicyCacheTTL(0)` disables the cache. The raw `Scoped.Store()`
(no guard) remains for trusted internal callers; the admin plane and tests use
unguarded stores directly.

Proxy mode uses a dedicated seam: `Registry.AuthorizedProxyStore` authorizes the
subject against the **reader** policy once, up front, and then returns an
*unguarded* store. Pull-through is a read from the client's perspective —
clients only `GET`, and the surface populates the cache (both `.cache/` metadata
and real artifact `File`s) on their behalf — so reader policy gates the whole
operation and the unguarded store keeps cache-fill writes from being rejected as
`OpWrite`. This is why a namespace with only a reader policy can populate a proxy
cache.

The `echo` surface (`pkg/surface/echo`, mounted only for `--repo-type=echo`) is
a diagnostic — not a package format — that drives this whole stack
(credential extraction → OIDC verification → 401 challenge → namespace authz)
for end-to-end testing, including a real GitHub Actions OIDC token in the
`oidc-e2e` workflow.

## Observability

`pkg/observability` wraps each plane's handler (`observability.Wrap`) and
intercepts `GET`/`HEAD /healthz`, `GET`/`HEAD /readyz`, and the configured
metrics path **before** auth or format routing, so probes and scrapes never
require credentials. Everything else flows through a request middleware that
records one HTTP metric and one structured log line per request.

- **Liveness** (`/healthz`) returns `200`/`ok\n` (empty body for `HEAD`) with
  no backend call.
- **Readiness** (`/readyz`) runs a backend `Pinger` with a **2s timeout** and
  caches a *successful* result for **1s** per handler instance (failed probes
  are not cached) to avoid probe storms. The data plane's pinger proves the
  bucket is reachable (a sentinel `Exists` under the deployment root); the
  admin plane's proves the namespace catalog is listable
  (`namespace.Store.Ping`). A nil pinger collapses readiness to liveness
  (`200`). The body reports `status`, `backend`, and build identity
  (`version`, `commit`, `os_arch` from `internal/version`); a failed probe
  returns `503` with a generic, secret-free `error` and logs the real cause —
  signed URLs and credentials never reach the response.
- **Metrics** are served on `--metrics-path` (default `/metrics`) only when
  `--enable-metrics` is set; otherwise the path returns `404` and the recorder
  is `metrics.NoOp()`, so there are no nil checks and no runtime panics.

Metrics use the `open_artifact_` prefix via a `metrics.Recorder`
(`NoOp()` / `NewPrometheus(reg)`; a nil registry seeds Go/process collectors).
The recorder is created once in the command layer and shared two ways: the
observability middleware records `http_*` series (labels `format`, `op`,
`status`), and the namespace `Registry` installs it on every scoped
`core.Store` so blob backend calls (`blob_backend_*`, labels `op`, `status`)
and download-redirect outcomes (`blob_redirect_total`, label `outcome`:
`redirected`/`inline`/`error`) are instrumented.

The HTTP `format`/`op` labels come from a mutable request state on the context.
A nested router labels its format with `observability.WrapWithFormat(format,
…)` and a route names its operation with `observability.SetOperation(r, op)`;
unset labels fall back to `unknown` (format) and a method map (GET/HEAD→`read`,
PUT/POST/PATCH→`write`, DELETE→`delete`, else lowercased method). A response
wrapper records the status (defaulting to `200` when a handler writes a body
without `WriteHeader`) and response bytes; request bytes come from
`Content-Length` (0 for unknown/chunked — the body is never read).

**Blob instrumentation respects core's purity.** `pkg/core`/`blobstore` must
not import `pkg/metrics`, so `blobstore` defines a small, metrics-agnostic
`Metrics` hook (the same shape as `Guard`) installed via
`blobstore.WithMetrics`; a `metrics.Recorder` satisfies it structurally. The
Store wraps each bucket primitive (`exists`, `read_all`, `write_all`,
`new_reader`, `new_writer`, `writer_close`, `list`, `attributes`,
`signed_url`) with timing + a status classified from the gocloud error code
(`ok`/`not_found`/`already_exists`/`unsupported`/`error`), preserving the
existing error wrapping and sentinel mapping. `File.DownloadURL` emits
`BlobRedirect("redirected")` for a signed URL, `"inline"` when signing is
unsupported (empty URL), and `"error"` when signing fails and the caller falls
back to streaming.

Structured request logs (via `pkg/logging`) carry `method`, `path`, `status`,
`duration_ms`, `bytes_in`, `bytes_out`, `format`, `op`, optional `namespace`
and `request_id` (`X-Request-Id`), and an `error` only when one is explicitly
recorded with `observability.RecordError`. The query string is omitted and the
`Authorization` header is never logged.

## Proxy primitives

Shared, format-agnostic pull-through machinery, built so proxy-mode format
surfaces can compose it without depending on each other. The HTTP client,
negative cache, singleflight, and filters live under `pkg/proxy`; the
metadata blob cache lives on the `core` nouns themselves (see below).

- **`pkg/proxy/httpclient`** — a context-aware upstream client with context-aware
  `Get`/`Head`, a configurable buffered-body cap (oversized bodies fail with
  `ErrOversized` before unbounded buffering), and status-mapping helpers
  (`IsOK`/`IsNotFound`/`IsServerError`) so a clean upstream 404 is distinguishable
  from an unavailable or malformed upstream. An HTTP status is never an error;
  errors are reserved for transport failure, cancellation, oversize, or read
  failure. Its default transport assumes **HTTP/2** (negotiated over TLS via
  ALPN) and ships sane defaults — a warm per-host idle connection pool and
  dial/TLS/response-header/idle timeouts; there is no blanket client timeout
  (overall deadlines come from the request context, so artifact streams aren't
  capped). The underlying `*http.Client` is injectable for tests. It carries no
  package-format behavior.
- **the `.cache/` cache lives on the `core` nouns as Files**, following the File
  verb pattern: `AddCache(ctx, key, body)` writes (like `AddFile`) and
  `Cache(key)` returns a no-I/O `core.CacheFile` handle (like `File`) on `Store`
  (format level), `Package`, and `Version`. A cache file is the same
  blob+`.meta` storage and read/digest code as a regular File, under a reserved
  `.cache/` folder, keyed by a logical name (`encodeSegment`-escaped, e.g.
  `simple:requests`). `AddCache` is mutable (overwrites) and the handle is
  evictable (`Delete`); freshness comes from `Meta.UpdatedAt`. It caches only
  **derived index/metadata**; artifact bytes are written as real
  Packages/Versions/Files and served like hosted content. Cache files never
  appear in listings, and the format-level cache is fully invisible to
  `Packages`. Cache ops authorize as **reads**, so reader policy is sufficient
  (the guard maps cache fill to `OpRead`).
- **`pkg/proxy/negcache`** — an in-memory, process-local negative cache for
  repeated upstream 404s, keyed by `(namespace, format, logical-key)` with a
  short default TTL (~30s). It is reconstructible and never persisted to the
  bucket.
- **`pkg/proxy/singleflight`** — a typed wrapper over
  `golang.org/x/sync/singleflight` that collapses concurrent cold misses for one
  key into a single fill per process. Different replicas may each fetch; that is
  acceptable for a pull-through cache.
- **`pkg/proxy/filter`** — the ordered allow/deny/delay filter chain: the
  persisted config schema (`Spec`/`Rule`), its validation, and the decision
  engine (`Ref` → `Decision`). The schema lives here as the single source of
  truth; `pkg/namespace` embeds `filter.Spec` in its proxy block and validates
  the chain through `filter.Validate` during spec validation (so `namespace`
  depends on this pure, stdlib-only leaf, not the reverse). Kinds: `allow`/`deny`
  match `path.Match` globs (so `*` does not cross `/`, which matters for npm
  scoped names) and/or package/version rules — first decision wins, abstain
  moves on, all-abstain defaults to allow; `delay` quarantines artifacts younger
  than `min_age` and asks for metadata when the publish time is unknown. Filters
  apply only to artifact/file downloads, never to index/metadata listings.

On a cold miss the surface fetches from upstream and **writes artifact files
into the namespace's `core.Store` as real Packages/Versions/Files**, so they are
served from our bucket thereafter exactly like hosted content; only derived
index/metadata lands in `.cache/`. Cold-miss bytes flow through open-artifact
(never redirect clients to public upstream URLs); a Store-hosted artifact may
still use backend signed-URL redirects because those target the
operator-controlled bucket. Reader policy is sufficient to populate both the
Store and the cache.

The hosted PyPI simple-project page also has a small **process-local rendered
HTML/JSON source cache**. It is intentionally not durable state: the bucket
remains the source of truth, successful uploads invalidate only the affected
project on the serving process, and each in-flight render records a generation
so an invalidation that happens while it is loading cannot store stale output.
Other replicas may serve an older rendered page until their TTL expires.

### PyPI proxy mode

A PyPI namespace with `mode: proxy` is a pull-through cache of
`Spec.Proxy.Upstream` (e.g. `https://pypi.org`). The surface reads `Spec.Mode`
per request and dispatches: hosted routes behave as before; proxy routes reject
uploads with `405 Method Not Allowed` (`Allow: GET, HEAD`) and serve reads from
the cache, filling from upstream on a miss. Upstream URLs are built by trimming
the configured base and appending the PyPI layout (`/simple/`,
`/simple/<project>/`, and `/pypi/<project>/<version>/json` for delay metadata).

- **Project index** (`/simple/<project>/`) uses a two-level cache. An in-process
  memo (default 10s) absorbs bursts. On a memo miss the surface goes to upstream
  — coalesced per `(namespace, project)` with singleflight — parses the document
  as PEP 691 JSON or PEP 503 HTML (chosen by response content type), distills it
  to a canonical `{filename, upstream_url, sha256, requires-python, version,
  upload-time}` per file (relative links resolved against the simple URL, version
  derived from the filename), and **write-through**s that snapshot to the durable
  `.cache/` entry keyed `pypi:simple:<project>`. File links are rewritten back
  through open-artifact when rendered, and the response is HTML or PEP 691 JSON
  by the same `Accept` negotiation as hosted. The durable snapshot is **read only
  when upstream is unavailable** (so while upstream is reachable it is written but
  never served): on a refresh failure the surface serves the durable snapshot at
  any age, else a minimal index synthesized from locally cached files, else
  returns `503`. A clean upstream `404` is remembered in the negative cache
  (default 30s) and returned as `404`. Artifact bytes are cached differently —
  always pulled on first request and served from our storage thereafter.
- **File download** (`/packages/<project>/<version>/<filename>`) serves the
  local `File` when present (exactly like hosted). On a miss it resolves the
  upstream URL from the cached index, evaluates the namespace filter chain
  (`Ref{Package, Version, PublishedAt}`; a delay filter with an unknown publish
  time fetches the upstream per-release JSON, then fails closed if still
  unknown; a deny is logged and returned as `404`), then **streams** the bytes
  through open-artifact (never redirecting clients to the public upstream) with
  a **tee into the Store** as a real `Package`/`Version`/`File` — no
  full-artifact buffering. The client is the primary consumer: a Store-write
  failure is swallowed so it cannot truncate the client response (governed by
  upstream stream success), and is logged. If the client disconnects (or
  upstream ends early), the Store write aborts cleanly — `blobstore` cancels the
  in-flight blob write so no truncated, servable `File` is committed to poison
  the cache; the next request refills. `HEAD` answers existence from index
  metadata without fetching or caching bytes. We do **not** verify the bytes
  against the index-advertised `sha256` — that hash shares the upstream's trust
  — but record it (`pypi:upstream_url`, `sha256`) and re-serve it so clients
  (pip) verify end to end; the local `File`'s own digest is authoritative.

The simple **root** (`/simple/`) in proxy mode lists only locally cached
projects rather than proxying the upstream's full registry listing.

### npm hosted surface

A hosted npm namespace (`pkg/surface/npm`) speaks the npm registry protocol
over the same namespace-scoped `core.Store`, composing the shared `surface`
helpers exactly as PyPI does — only the wire protocol and codec are
npm-specific. Routes live under `/{namespace}`: packument read and publish at
`/{name}` and `/@{scope}/{name}`; tarball download at `…/-/{file}.tgz`;
dist-tags under `/-/package/{name}/dist-tags[/{tag}]`; and the `/-/ping` and
registry-root probes. Scoped names arrive either as `@scope/name` or
`%2f`-encoded; both decode to the same route.

- **Codec.** The format owns the package-name mapping: an unscoped `left-pad`
  becomes the core package `u/left-pad` and a scoped `@scope/pkg` becomes
  `s/scope/pkg`. `blobstore` escapes the embedded `/` into a single path-safe
  bucket segment and round-trips it through listing, so the two namespaces
  never collide and never begin with a reserved `.`. The original npm name is
  recorded in the package/version annotations (`npm:name`). Names are validated
  to npm's rules (≤214 chars, lowercase, `[a-z0-9._-]` per segment, no leading
  `.`/`_`, no `~`/space/traversal); the version string is used verbatim as the
  core version.
- **Publish.** `PUT /{name}` carries the CouchDB-shaped document (`name`,
  `versions`, `_attachments`, optional `dist-tags`). The body is capped by
  `--npm-max-upload-bytes`; v1 accepts exactly one version per publish. The
  surface decodes the base64 attachment, recomputes the SHA-1 hex and SHA-512
  SRI and compares them to the declared `dist.shasum`/`dist.integrity` when
  present (mismatch → 422), rewrites `dist.tarball` to point back at
  open-artifact, then stores two files under the core version: the tarball
  (original `<unscoped>-<version>.tgz` name) and `package.json` (the per-version
  metadata used to rebuild packuments). The tarball write is the immutability
  gate — a republish of an existing version collides as 409. Dist-tags from the
  request are applied via `Package.SetTag`, defaulting `latest` to the published
  version when the client sent none. Publish is a pure write path (no reads), so
  a write-only subject can publish.
- **Packument.** `GET /{name}` lists the package's versions, reads each stored
  `package.json`, rewrites every `dist.tarball` to the current request host and
  namespace (honoring `X-Forwarded-Proto/Host`), and assembles `_id`, `name`,
  `versions`, `dist-tags`, and best-effort `time` (from the `npm:uploaded_at`
  annotation). A missing package is 404.
- **Tarball download.** The version is derived from the npm filename convention
  (`<unscoped>-<version>.tgz`) for a direct lookup, falling back to a scan of the
  package's versions, then served through the shared `RedirectOrStreamFile`
  (signed-URL redirect when the backend supports it, else streamed).
- **Dist-tags.** `GET …/dist-tags` resolves every tag to its target version;
  `PUT`/`POST …/dist-tags/{tag}` sets one (404 if the target version is absent,
  so no dangling tags — this read+write check means dist-tag mutation needs both
  reader and writer policy); `DELETE` returns 501 in v1.
- **Modes/auth.** Reads authorize `OpRead`, publish and dist-tag mutation
  `OpWrite`, all enforced below the surface inside the guarded Store. Proxy
  namespaces are handled separately (the npm proxy is planned): until then a
  proxy-mode write returns 405-equivalent rejection and reads map to
  `ErrUnsupported` (501).

## gocloud.dev/blob notes

- Open buckets from a URL (`blob.OpenBucket(ctx, "s3://…")`, `mem://`,
  `file:///…`) so the backend is a deployment flag. Register drivers
  (`memblob`, `fileblob`, `s3blob`, `gcsblob`, `azureblob`) in the command
  layer only — never in `pkg/core`.
- The command owns bucket lifecycle: open once at startup, close on shutdown.
  `blobstore.Store` must not close a caller-owned bucket.
- Streaming upload: `bucket.NewWriter` + rolling SHA256 on the write path;
  write the `.meta.<file>` sidecar after the writer closes successfully. The
  writer is created under a cancelable child context; on a mid-stream read error
  the write is **cancelled** before `Close` (for memblob/fileblob a plain
  `Close` would commit the bytes written so far as a complete object), so a body
  that ends early — a disconnected upload, an interrupted proxy stream — never
  leaves a partial, servable blob.
- `File.DownloadURL` wraps `bucket.SignedURL` behind a mandatory,
  facade-transparent LRU + singleflight cache with per-cloud TTL parsing.
  memblob/fileblob return no signed URL → cache the miss and return an empty
  URL (nil error) so the surface falls back to streaming `File.Read`.
- `bucket.List` with a delimiter gives the directory children for the listing
  verbs; sort caller-side if order matters.

## The CLI (implemented)

Single binary `open-artifact`; cobra for commands, viper for flag/env
resolution. Subcommands: `serve` (data plane, default port 8080) and
`admin serve` (control plane, default port 8081). `--version` prints version,
commit, and `GOOS/GOARCH`. Every flag has a matching env var (prefix
`OPEN_ARTIFACT`, dashes → underscores; platform `PORT` → `--port`, with
`OPEN_ARTIFACT_PORT` taking precedence); no config files. The root command uses
`SilenceUsage`/`SilenceErrors` so errors are testable; each command validates
config at startup and fails with clear, joined errors. Data-plane-only flags
(`--repo-type`, `--disable-authn`, `--authn-*`) live on `serve` and are stubbed
here for later issues. The command layer (`pkg/command`) is the only place that
opens a bucket and registers blob drivers (`pkg/bucket`). The logger
(`pkg/logging`) is built once at command start, placed on the context, and
flows down from there; the shared HTTP server lifecycle (`pkg/serving`) reads it
from the context and a middleware injects it onto every request's context. The
`artctl` client (deferred to post-parity) talks HTTP only and never opens a
bucket.

## Adding a new surface

**Consistency across surfaces is a first-class requirement.** PyPI, npm, and
Maven must feel like one product, not three. A surface only owns the part that
is genuinely format-specific — its wire protocol and codec. Everything else
(namespace lookup and authz, hosted/proxy dispatch, error→HTTP mapping,
redirect-or-stream downloads, metrics/op labeling, upload caps, cache TTL
handling) goes through the **shared `surface` framework** so all formats
behave identically. If two surfaces need the same behavior, factor it into the
shared layer rather than copying it; if you must diverge, say why in the code
and the docs. Match the existing surfaces' naming, flag/env conventions, error
shapes, and test layout.

1. New `pkg/surface/<format>` package: inbound protocol handler + outbound
   upstream client. Construct it from the **namespace registry**, never a raw
   `*blobstore.Store`.
2. Support both modes: hosted (accept uploads, immutable writes unless
   `--allow-overwrite`) and proxy (writes disabled, pull-through via the proxy
   primitives). Read `Spec.Mode` per request so admin mode switches take
   effect without restart.
3. Format codec maps the wire protocol to logical package/version/file/tag
   names and passes them through raw — `blobstore` owns path-safe encoding and
   rejects empty/leading-dot names (`ErrInvalidName`), so the codec does not
   sanitize names; it just maps that and the other
   `core`/`namespace`/`auth` sentinels to the format's HTTP error shape via the
   shared `surface` helpers.
4. Use the shared helpers: JSON/error writers, `RedirectOrStreamFile`, HEAD
   handling, `MaxBytesReader`, metrics op labeling.
5. Unit tests for the codec + handler against `mem://`; integration tests use
   real package-manager clients against the harness.
6. Operator notes in `docs/`; flags/env for upstream URL, upload caps, and
   cache TTLs.

## Non-goals / deferred (v1)

- **Deletion / yank / unpublish of packages** — semantics differ across
  formats; a separate design pass. (Deleting an *empty* namespace is supported;
  emptiness is derived from listing, not a side index.)
- **Multi-replica `SetTag`** — v1 guards the per-tag read-modify-write with an
  in-process mutex; multi-replica needs CAS / conditional writes at the bucket
  layer.
- **Static username/password auth** — OIDC only in v1.
- **Serving multiple formats from one process** — v1 serves exactly one format
  per `serve` process; run separate processes for separate endpoints.
- **SSE-C / per-blob-key encrypted buckets**, cross-region failover, external
  blob paths — revisit when a concrete requirement lands.
- **The `artctl` client binary** — server first; deferred to post-parity.
