# AGENTS.md

How we work on **open-artifact** — the principles and conventions for changing
this codebase. It is deliberately high level. The detailed design lives in
[`docs/architecture.md`](docs/architecture.md); the roadmap lives in GitHub
issues.

open-artifact is a lightweight, stateless, multi-format artifact registry
(PyPI, npm, Maven) backed by a single `gocloud.dev/blob` bucket — no database,
no second metadata store. Read `docs/architecture.md` before making
non-trivial changes.

## Think from first principles

Derive decisions from the problem and the architectural invariants, not from
precedent or from what some document literally says. The invariants in
`docs/architecture.md` (one blob backend, a pure `core`, the namespace as the
canonical partition, one product across formats) are the fixed points; reason
forward from them. When a requirement seems off, surprising, or more complex
than the problem warrants, question it before building it — the cheapest code
is the code you talk yourself out of writing. Prefer the simplest design that
honors the invariants.

## Tracking work: issues are the roadmap

The roadmap lives in **GitHub issues**, not in this repo's docs. Each issue is
self-contained — it carries its own scope, dependencies, and acceptance
criteria. Read an issue (and the ones it depends on) before starting, and keep
status there. Open a tracking issue for non-trivial work.

**Treat each issue with a grain of salt.** An issue is a snapshot of intent at
the time it was written; it can be stale, incomplete, or wrong. Reconcile it
against `docs/architecture.md` and the actual code, and if first-principles
thinking points to a better path, take it — then update the issue and the
architecture doc to match. The architecture doc is the source of truth for
*design*; issues are the source of truth for *what to do next*.

## Keep the architecture doc alive

`docs/architecture.md` is a living document. When behavior or structure
changes, update it in the same change — stale design docs are worse than none.
If you discover the doc and the code disagree, resolve it (fix one, note why)
rather than leaving the contradiction. New cross-cutting design decisions
belong there, not buried in a commit message or an issue comment.

## Coding principles

- **Go only**, unless there's a strong reason. Follow Effective Go.
- **Explicit, boring code over clever abstractions.** No premature generality;
  don't build for hypothetical future requirements.
- **Respect the dependency rule.** `pkg/core` is pure: no HTTP, auth,
  namespaces, upstreams, or metrics. Surfaces and commands compose those
  concerns around it; the arrows point one way. (Details in the architecture
  doc.)
- **One product, three formats.** PyPI, npm, and Maven must behave
  identically except where the wire protocol genuinely differs. Shared
  behavior goes in the shared `surface` framework, not copied per format; if
  you must diverge, justify it in code and docs.
- **Security and correctness first.** Validate at system boundaries (untrusted
  client input, upstream responses); trust internal invariants. Never log
  credentials.
- **Comments explain why, not what.** Default to none; add one only when a
  constraint or hazard isn't obvious from the code.

## Harness principles

- **One binary, flags + env, no config files.** `open-artifact` with `serve`
  and `admin serve`. Every flag has a matching env var (prefix
  `OPEN_ARTIFACT`); also bind platform `PORT`. Validate config at startup and
  fail with clear, joined errors.
- **Build and test via the `Makefile`;** releases via goreleaser.
- **Always sign off commits** (`git commit -s`) — the repo enforces DCO.

## Testing standards (non-negotiable)

- **Unit + integration for every feature, in the same change.** Tests are not
  a follow-up.
- Integration tests live behind a `//go:build integration` tag and run the
  real layers against **`mem://`** (and where relevant **`file://`** via
  `t.TempDir()`) buckets. **No mock `Store`** for storage or surface behavior —
  use fakes, exercise real layers together.
- `t.Parallel()` and `t.Context()` in every test, unless a documented
  process-global exception applies (e.g. env-var or registry mutation).
- Table-driven tests for multiple inputs; compare with `cmp.Diff`.
- Cover the hard cases: concurrent writes to one version, write/read races,
  tag races, empty-scope listings, leading-dot/`..` rejection at the codec,
  digest mismatch, cross-namespace isolation, deny-all policy, proxy
  stale-fallback, filter ordering.
- End-to-end surface tests drive a real client (`pip`/`twine`, `npm`, `mvn`)
  against an in-process server. Do **not** require Docker — the backend is
  memblob/fileblob; only the package-manager client tools need to be
  installed.
- Every change: `go test -race ./...`; for storage/surface work also
  `go test -race -tags=integration ./...`.

## Predecessor code is historical context only

open-artifact is the spiritual successor to `yolocs/ocifactory`. You may study
ocifactory while *planning* — to understand a format's wire protocol or recall
a design trade-off — but every detail needed to *implement* open-artifact must
live in this repo. Do not cite ocifactory as a source of truth, copy its
storage model, or assume its OCI backend. If something is missing here, add it
to `docs/architecture.md`.
