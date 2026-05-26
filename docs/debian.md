# Debian (APT) Surface

The Debian surface serves Debian/APT repositories from a namespace-scoped
`core.Store`. Start it with `--repo-type=debian`; all repository routes live
under the namespace name.

The Debian surface is **proxy-only**. Debian has no interoperable "publish a
`.deb` over HTTP" protocol — packages are published by regenerating and
GPG-signing the repository indexes with tools like `reprepro`/`aptly` — so a
Debian namespace is a pull-through cache of an upstream APT repository (e.g.
`https://deb.debian.org/debian`, `http://archive.ubuntu.com/ubuntu`). Every
write method returns `405`.

Point APT at the namespace with a `sources.list` entry whose URI is
`https://artifact.example/{namespace}/debian`, for example:

```
deb https://artifact.example/team-a/debian bookworm main
```

Create the namespace in proxy mode with the upstream repository root as
`proxy.upstream`.

## What is cached and how

A request path after `/{namespace}/debian/` is the upstream-relative repository
path. The surface classifies it by its top-level directory:

- **`dists/...` — index files** (`InRelease`, `Release`, `Release.gpg`, the
  per-component/architecture `Packages[.gz|.xz]`, `Sources`, `Contents`,
  `by-hash`, …). These are pulled from upstream and cached, and served
  **byte-for-byte identical to upstream** — open-artifact never parses,
  rewrites, re-signs, or synthesizes them. APT verifies them end-to-end against
  the GPG key you configure, so verbatim passthrough is what makes verification
  work. Each request fetches upstream and streams the bytes through while teeing
  them into a durable cache; the cached copy is served **only as a stale
  fallback** when upstream is unreachable or returns 5xx (otherwise `503`). A
  clean upstream `404` is negative-cached.
- **`pool/...` — artifacts** (`*.deb`, `*.udeb`, `*.dsc`, `*.tar.*`, …). Pulled
  on first request — streamed through open-artifact (never a redirect to
  upstream) with a tee into the bucket — and served from our storage thereafter,
  surviving a restart.

GPG signing is unchanged: APT trusts the upstream's key, which you configure on
the client exactly as you would for the upstream directly (or use
`[trusted=yes]` for an unsigned/private mirror).

## Filters

The namespace filter chain applies to **pool artifacts only**, never to index
files. `allow`/`deny` match the package name (parsed from the `.deb`/source
filename). Debian exposes no reliable per-file publish time, so a `delay` filter
that needs one **fails closed** (the artifact is denied / served as `404`).

## Authorization

Reads require reader policy (`OpRead`); a reader-only namespace can populate the
pull-through cache. All writes return `405`.

## Runtime flags

- `--debian-proxy-negative-cache-ttl` /
  `OPEN_ARTIFACT_DEBIAN_PROXY_NEGATIVE_CACHE_TTL`: how long an upstream `404`
  (index or artifact) is remembered. Default `30s`; `0` uses the default.

## Client example

```bash
echo 'deb https://artifact.example/team-a/debian bookworm main' \
  | sudo tee /etc/apt/sources.list.d/open-artifact.list
sudo apt-get update
sudo apt-get install hello
```
