# open-artifact

A lightweight, stateless, multi-format artifact registry that uses any
[`gocloud.dev/blob`](https://pkg.go.dev/gocloud.dev/blob) bucket as its sole
storage backend. A small, open-source alternative to Artifactory or Nexus for
teams who already have an object store and don't want to run a database to
front it.

Speaks native package-manager protocols — **PyPI, npm, and Maven** — and
stores everything (blobs, API objects, dist-tags, namespace metadata, proxy
caches) as plain objects in one bucket (S3 / GCS / Azure Blob / filesystem /
in-memory). The bucket's directory tree *is* the index: no database, no
separate metadata store.

> Successor to [`yolocs/ocifactory`](https://github.com/yolocs/ocifactory),
> which used an OCI registry as its backend. open-artifact keeps the formats,
> CLI philosophy, and testing bar; it swaps the storage layer for
> `gocloud.dev/blob`. OCI is lineage, not a backend requirement.

## How it works

- **Namespaces.** Every package-manager URL is rooted at `/{namespace}/...`.
  A namespace is a control-plane object — stored in the same bucket — with its
  own mode and access policy.
- **Hosted or proxy.** A hosted namespace accepts client uploads; a proxy
  namespace is a pull-through cache of an upstream registry (PyPI,
  npmjs.org, Maven Central) that fills the bucket on cold reads.
- **Auth.** Clients authenticate with OIDC tokens; each namespace policy
  decides whether the subject may read or write.
- **Two planes.** A data plane (`open-artifact serve`) speaks one package
  format per process; an admin plane (`open-artifact admin serve`) manages
  namespaces. Both expose `/healthz`, `/readyz`, and Prometheus `/metrics`.

## Status

Core storage substrate (`pkg/core` + `pkg/core/blobstore`) is implemented:
chainable noun handles, streaming uploads with rolling SHA256, listings,
dist-tags, and a cached signed-URL redirect. The runtime, namespaces, auth,
observability, proxy primitives, and the PyPI/npm/Maven surfaces are tracked
as the **parity** issues on GitHub. See
[`docs/architecture.md`](docs/architecture.md) for the design and
[`AGENTS.md`](AGENTS.md) for how we work on the codebase.

## Layout

```
cmd/open-artifact   single binary: `serve` (data plane) + `admin serve` (control plane)
pkg/core            data nouns + scope-blind Store interface + Meta + errors
pkg/core/blobstore  core.Store over a gocloud.dev/blob bucket
pkg/namespace       namespace model, blob-backed catalog, data-plane factory
pkg/auth            OIDC authn + per-namespace authz
pkg/surface         per-format inbound handler + upstream client (pypi, npm, maven) + admin
```

See [`docs/architecture.md`](docs/architecture.md) for the design — invariants,
dependency rule, storage model, and the per-surface contract.

## Develop

```sh
make build            # build the binary
make test             # unit tests (race)
make test-integration # real package-manager client tests
```

## License

Apache-2.0. See [LICENSE](LICENSE).
