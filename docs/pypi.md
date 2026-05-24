# PyPI Surface

The PyPI surface serves Python packages from a namespace-scoped `core.Store`.
Start it with `--repo-type=pypi`; all package routes live under the namespace
name. A namespace serves in **hosted** mode (accepts uploads, serves what was
published) or **proxy** mode (a pull-through cache of an upstream PyPI). The mode
is read per request from the namespace spec, so admin mode changes take effect
without a restart.

## Routes

- `POST /{namespace}/` and `PUT /{namespace}/` accept legacy PyPI multipart
  uploads from `twine upload`.
- `GET|HEAD /{namespace}/simple` and `/{namespace}/simple/` return the PEP 503
  root simple index.
- `GET|HEAD /{namespace}/simple/{project}/` returns the per-project simple
  index. Send `Accept: application/vnd.pypi.simple.v1+json` to receive the
  PEP 691 JSON shape.
- `GET|HEAD /{namespace}/packages/{project}/{version}/{filename}` downloads a
  hosted wheel, `.tar.gz`, or `.zip`.

Project names are normalized with PEP 503 rules at upload and read time. Names
starting with `.`, path separators, traversal, and unsupported file extensions
are rejected.

## Proxy mode

A namespace with `mode: proxy` and `proxy.upstream` set (for example
`https://pypi.org`) is a pull-through cache:

- Uploads (`POST`/`PUT`) return `405 Method Not Allowed` with `Allow: GET, HEAD`.
- `GET|HEAD /{namespace}/simple/{project}/` serves the upstream simple index,
  rewriting file links back through open-artifact. The index is parsed from
  PEP 691 JSON or PEP 503 HTML. Caching is two-level: a short in-process cache
  absorbs bursts, and a durable snapshot is kept for when upstream is down. While
  upstream is reachable the snapshot is refreshed (write-through) but the response
  comes from upstream; if upstream is unavailable the surface serves the durable
  snapshot, then a minimal index synthesized from already cached files, and
  finally returns `503`. A clean upstream `404` returns `404`.
- `GET /{namespace}/packages/{project}/{version}/{filename}` serves a cached file
  if present, otherwise fetches it from upstream through open-artifact, caches it,
  and serves it. The namespace's proxy filter chain (`allow`/`deny`/`delay`) is
  evaluated on downloads; a denied artifact returns `404`. The upstream-advertised
  `sha256` is recorded and re-served in the index for clients to verify against,
  but is not re-checked here (the hash and bytes share one upstream source).
- `GET|HEAD /{namespace}/simple/` lists only locally cached projects, not the
  upstream's full registry listing.

Reader policy is sufficient to populate a proxy cache — the surface writes
cached files on the caller's behalf. Clients only ever read.

## Runtime Flags

- `--pypi-max-upload-bytes` / `OPEN_ARTIFACT_PYPI_MAX_UPLOAD_BYTES`: maximum
  multipart upload body size. The default is `104857600` bytes; `0` uses the
  default cap.
- `--pypi-simple-index-cache-ttl` /
  `OPEN_ARTIFACT_PYPI_SIMPLE_INDEX_CACHE_TTL`: per-process cache TTL for
  project simple indexes (both hosted and proxy). The default is `60s`; `0`
  disables caching.
- `--pypi-proxy-index-cache-ttl` /
  `OPEN_ARTIFACT_PYPI_PROXY_INDEX_CACHE_TTL`: in-process cache TTL for the proxy
  upstream index (the burst absorber). The default is `10s`; `0` uses the
  default, a negative value disables it. The durable upstream-index snapshot has
  no TTL — it is overwritten on every successful refresh and read only as a
  fallback when upstream is unavailable.
- `--pypi-proxy-negative-cache-ttl` /
  `OPEN_ARTIFACT_PYPI_PROXY_NEGATIVE_CACHE_TTL`: how long an upstream `404` is
  remembered. The default is `30s`; `0` uses the default.
- `--pypi-proxy-max-artifact-bytes` /
  `OPEN_ARTIFACT_PYPI_PROXY_MAX_ARTIFACT_BYTES`: cap on the buffered upstream
  artifact during a proxy cache fill, as a memory safety bound. The default is
  `1073741824` bytes; `0` uses the default.

Successful uploads invalidate only the affected project on the local process.
Multi-replica deployments may serve stale project indexes until the TTL expires.

## Client Examples

```bash
twine upload \
  --repository-url https://artifact.example/team-a/ \
  --username "$OPEN_ARTIFACT_USER" \
  --password "$OPEN_ARTIFACT_TOKEN" \
  dist/*

pip download \
  --index-url https://artifact.example/team-a/simple/ \
  --no-deps \
  demo-pkg==0.1.0

pip install \
  --index-url https://artifact.example/team-a/simple/ \
  demo-pkg==0.1.0
```
