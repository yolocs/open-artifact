# PyPI surface

The PyPI surface (`pkg/surface/pypi`) speaks the Python package-index protocol
on the front and a `core.Store` on the back. It serves PEP 503 / PEP 691 simple
indexes, accepts `twine`-style uploads, and — when an upstream is configured —
proxies and caches through to a real PyPI.

## Endpoints

Mounted under an operator-chosen prefix (for example `/pypi`):

| Method     | Path                                         | Purpose                          |
| ---------- | -------------------------------------------- | -------------------------------- |
| `GET`      | `{prefix}/simple/`                           | Root project list (PEP 503/691)  |
| `GET`      | `{prefix}/simple/{package}/`                 | Per-project file index           |
| `GET/HEAD` | `{prefix}/packages/{package}/{version}/{file}` | File download                  |
| `POST`     | `{prefix}/`                                  | `twine` upload                   |

Content negotiation follows PEP 691: a request whose `Accept` prefers
`application/vnd.pypi.simple.v1+json` gets JSON; everything else (including no
`Accept`) gets HTML, keeping pre-PEP-691 clients working.

## Client configuration

Install (read):

```
pip install --index-url http://HOST/pypi/simple/ <package>
```

Publish (write):

```
twine upload --repository-url http://HOST/pypi/ dist/*
```

## Name handling and the leading-dot rule

Package names are normalized per PEP 503 (lowercase; runs of `.`, `-`, `_`
collapse to a single `-`), so a `twine upload Foo_Bar` is found by
`pip install foo-bar`. The codec **rejects any package, version, or file name
beginning with `.`**: the Store reserves leading-dot names at every directory
level and silently drops them from listings, so accepting one would let a
publish vanish. Filenames are further constrained to a path-safe character set
with no separators.

## Proxy / cache-through

When constructed with an `UpstreamClient`, the surface becomes a caching proxy:

- **Index** — `{prefix}/simple/{package}/` merges locally stored files with the
  upstream's PEP 691 JSON listing. Every entry's download URL is rewritten to
  route back through this surface, so file fetches flow through the cache. The
  surface re-renders the index from structured data rather than rewriting
  upstream HTML.
- **Download** — on a local miss the surface resolves the file's upstream URL
  from the project index, streams the bytes to the client, and **tees the same
  bytes into the Store** (a synchronous tee, not an async backfill). The next
  request for that file is served locally. A client disconnect mid-stream
  aborts the partial cache write; the next request retries. Concurrent
  cold-misses for the same file each fetch upstream; the first `AddFile` wins
  and the loser falls back to serving the now-cached copy (no duplicate blob,
  disjoint paths never conflict).
- **HEAD** drains the upstream body into the Store and returns `200` without a
  body.

A degraded upstream does not take the whole project down: if the upstream
errors but local files exist, the index still serves the local view.

## Downloads: redirect vs. stream

`File.DownloadURL` is consulted first. On a backend that can sign URLs
(S3 / GCS / Azure) the surface answers a `GET` with a `307` redirect to the
signed URL so bytes never transit the server. On `memblob` / `fileblob`
(no signing) it streams the bytes directly. `HEAD` always short-circuits to a
presence check.

## Error mapping

`core.Store` sentinels map to HTTP via the shared `surface.WriteStoreError`
helper: `ErrNotFound` → 404, `ErrAlreadyExists` → 409 (a re-upload of an
existing file), `ErrDigestMismatch` → 422, `ErrUnsupported` → 501, anything
else → 500. Upstream failures map to 404 (upstream not-found) or 502 (upstream
unavailable / malformed). Internal error detail is never echoed to clients.

## Scope

The surface is bound to whatever scope its `core.Store` was constructed with
(for example `pypi/global`); the scope is never a request parameter. Deletion
and yank are out of scope for v1.
