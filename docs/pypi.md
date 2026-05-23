# PyPI Surface

The PyPI surface serves hosted Python packages from a namespace-scoped
`core.Store`. Start it with `--repo-type=pypi`; all package routes live under
the namespace name.

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

## Runtime Flags

- `--pypi-max-upload-bytes` / `OPEN_ARTIFACT_PYPI_MAX_UPLOAD_BYTES`: maximum
  multipart upload body size. `0` disables the limit.
- `--pypi-simple-index-cache-ttl` /
  `OPEN_ARTIFACT_PYPI_SIMPLE_INDEX_CACHE_TTL`: per-process cache TTL for
  project simple indexes. The default is `60s`; `0` disables caching.

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
