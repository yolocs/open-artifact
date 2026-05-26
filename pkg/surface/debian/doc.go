// Package debian implements the Debian/APT repository surface.
//
// Debian has no interoperable "publish a .deb over HTTP" protocol — packages
// are published by regenerating and GPG-signing the repository indexes with
// tools like reprepro/aptly — so this surface is proxy-only: a pull-through
// cache of an upstream APT repository (e.g. https://deb.debian.org/debian).
// Write methods always return 405.
//
// A request is rooted at /{namespace}/debian/<repo-path>, where <repo-path> is
// the upstream-relative path. Two subtrees matter:
//
//   - dists/...  the index files (InRelease, Release, Release.gpg, Packages[.gz|.xz],
//     Sources, Contents, by-hash, ...). These are served byte-for-byte from
//     upstream and cached: APT verifies them end-to-end against the client's
//     configured GPG key, so the proxy must never parse, rewrite, or re-sign
//     them. The durable .cache/ copy is a write-through served only as a stale
//     fallback when upstream is unavailable (the PyPI proxy shape).
//   - pool/...   the artifacts (*.deb, *.udeb, *.dsc, *.tar.*, ...). These are
//     cached as real Files: pulled on first request (streamed through with a
//     tee into the Store) and served from our bucket thereafter, exactly like
//     the Maven/PyPI/npm artifact path.
//
// Only the path codec and the index passthrough-with-cache are Debian-specific;
// everything else composes the shared surface/proxy machinery.
package debian
