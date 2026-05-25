# npm Surface

The npm surface serves npm packages from a namespace-scoped `core.Store`. Start
it with `--repo-type=npm`; all registry routes live under the namespace name. A
namespace serves in **hosted** mode (accepts `npm publish`, serves what was
published) today; **proxy** mode (a pull-through cache of an upstream npm
registry) is planned. The mode is read per request from the namespace spec, so
admin mode changes take effect without a restart.

Point a client at the namespace by setting its registry to
`https://artifact.example/{namespace}/`.

## Routes

All routes live under `/{namespace}`. Scoped packages may be sent as
`@scope/name` or `%2f`-encoded (`@scope%2fname`); both resolve identically.

- `GET|HEAD /{name}` and `/@{scope}/{name}` return the package **packument**.
- `PUT /{name}` and `/@{scope}/{name}` **publish** a package version.
- `GET|HEAD /{name}/-/{file}.tgz` (and the scoped form) download a **tarball**.
- `GET|HEAD /-/package/{name}/dist-tags` (and scoped) list **dist-tags**;
  `PUT|POST /-/package/{name}/dist-tags/{tag}` set one; `DELETE` returns `501`.
- `GET|HEAD /-/ping` returns `{}`; `GET|HEAD /` returns a minimal registry root.

## Naming and storage

Package names follow npm's rules: at most 214 characters, lowercase, each
segment limited to `[a-z0-9._-]` with no leading `.`/`_`, and no `~`, spaces, or
path traversal. Scoped names must be exactly `@scope/name`.

The format maps wire names to core package names — `left-pad` →
`u/left-pad`, `@scope/pkg` → `s/scope/pkg` — which the storage layer keeps as a
single, lossless, path-safe bucket segment. The original npm name is recorded in
annotations. Each published version stores two files: the tarball (under its
`<unscoped>-<version>.tgz` name) and a `package.json` holding the version
metadata used to rebuild packuments.

## Publish

`npm publish` sends a CouchDB-shaped document with `name`, `versions`,
`_attachments` (the base64 tarball), and optional `dist-tags`. The surface:

1. Caps the body with `--npm-max-upload-bytes`.
2. Verifies the URL package matches the document `name` (v1 accepts exactly one
   version per publish).
3. Streams the base64 tarball attachment straight into storage; the Store
   computes the SHA-1 and SHA-512 during the write and, when the publisher
   declared `dist.shasum`/`dist.integrity`, verifies them before committing — a
   mismatch aborts the write and returns `422`, leaving nothing stored. The
   decoded tarball is never held whole in memory.
4. Rewrites `dist.tarball` to point back at open-artifact, never at an upstream
   or publisher-provided host.
5. Stores the tarball and `package.json`, then applies dist-tags (defaulting
   `latest` to the published version when the client sent none).

Re-publishing an existing version returns `409`.

Packument reads rewrite every `dist.tarball` to the requesting host and
namespace, honoring `X-Forwarded-Proto`/`X-Forwarded-Host` (set these only from
a trusted front end).

## Authorization

Reads require reader policy (`OpRead`); publish requires writer policy
(`OpWrite`). Dist-tag mutation verifies the target version exists before setting
the tag (so no dangling tags), which means it needs both reader and writer
policy. A proxy-mode namespace rejects writes with `405` and reads with `501`
until the npm proxy lands.

## Runtime Flags

- `--npm-max-upload-bytes` / `OPEN_ARTIFACT_NPM_MAX_UPLOAD_BYTES`: maximum
  publish body size (the tarball is base64-wrapped in JSON, so the body is
  larger than the tarball). The default is `104857600` bytes; `0` uses the
  default cap.

## Client Examples

```bash
# Configure the registry (per-project .npmrc or npm config).
npm config set registry https://artifact.example/team-a/

npm publish                       # from a package directory
npm install left-pad@1.0.0
npm install @scope/pkg@2.0.0
npm dist-tag add left-pad@1.0.0 beta
npm dist-tag ls left-pad
```
