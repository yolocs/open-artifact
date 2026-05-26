// Package maven implements the Maven 2 repository surface in both hosted and
// proxy modes.
//
// Hosted namespaces accept deploys (PUT/POST) and serve them back. Proxy
// namespaces are read-only pull-through caches of an upstream repository
// (typically Maven Central): GET/HEAD reads are served and writes return 405.
//
// In proxy mode, maven-metadata.xml (artifact- and snapshot-level) and its
// checksum companions are fetched live from upstream on every request and
// streamed through unchanged — never cached or synthesized in v1 — so clients
// always see authoritative versioning. Immutable artifact files (.jar/.pom,
// sources/javadoc jars) are pulled on first request, teed into the blob Store,
// and served locally thereafter; a missing checksum companion is synthesized
// from a locally cached target or otherwise fetched from upstream, then cached.
// The negative cache bounds repeated upstream 404s.
//
// The archetype catalog (/archetype-catalog.xml) is local-cache-only in proxy
// mode: a catalog that is not already cached returns 404 (it is never fetched
// from upstream in v1), and writes return 405.
package maven
