# open-artifact

A lightweight, multi-format artifact registry that uses any
[`gocloud.dev/blob`](https://pkg.go.dev/gocloud.dev/blob) bucket as its sole
storage backend. A small, open-source drop-in for Artifactory or Nexus for
teams who already have an object store and don't want to run a database to
front it.

Speaks native package-manager protocols (PyPI and npm to start) and stores
everything — blobs, API objects, dist-tags — as plain objects in one bucket
(S3 / GCS / Azure Blob / filesystem / in-memory). The bucket's directory
tree *is* the index.

> Successor to [`yolocs/ocifactory`](https://github.com/yolocs/ocifactory),
> which used an OCI registry as its backend. open-artifact keeps the
> formats, CLI philosophy, and testing bar; it swaps the storage layer for
> `gocloud.dev/blob`.

## Status

Early scaffold — see [`docs/vision.md`](docs/vision.md) for the architecture
and milestones, and [`AGENTS.md`](AGENTS.md) for the design pillars,
storage model, and engineering standards. Open issues track the milestones.

## Layout

```
cmd/server     the server binary (cobra + viper)
cmd/client     the client binary (deferred)
pkg/core       data nouns + scope-blind Store interface + Meta + errors
pkg/core/blobstore  core.Store over a gocloud.dev/blob bucket
pkg/surface    per-format inbound handler + upstream client (pypi, npm)
```

## Develop

```sh
make build            # build both binaries
make test             # unit tests (race)
make test-integration # + integration tests against memblob/fileblob
```

## License

Apache-2.0. See [LICENSE](LICENSE).
