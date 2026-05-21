// Package bucket is the command-layer bucket opener. It registers the Go CDK
// blob drivers and opens a *blob.Bucket from a deployment URL. Driver imports
// live here, never in pkg/core, so the storage backend stays a deployment flag
// and core remains pure.
//
// The command owns bucket lifecycle: Open returns a cleanup function the caller
// must invoke on shutdown. blobstore.Store never closes a caller-owned bucket.
package bucket

import (
	"context"
	"fmt"

	"gocloud.dev/blob"

	// Blob drivers, registered by import for blob.OpenBucket URL dispatch.
	_ "gocloud.dev/blob/azureblob" // azblob://, Azure Blob URLs
	_ "gocloud.dev/blob/fileblob"  // file://
	_ "gocloud.dev/blob/gcsblob"   // gs://
	_ "gocloud.dev/blob/memblob"   // mem://
	_ "gocloud.dev/blob/s3blob"    // s3://
)

// Open opens the bucket identified by url and returns it alongside a cleanup
// function that closes it. The caller owns the returned bucket's lifecycle and
// must call cleanup on shutdown. cleanup is safe to call exactly once; it is
// nil only when Open returns an error.
func Open(ctx context.Context, url string) (*blob.Bucket, func(), error) {
	b, err := blob.OpenBucket(ctx, url)
	if err != nil {
		return nil, nil, fmt.Errorf("open bucket %q: %w", url, err)
	}
	cleanup := func() { _ = b.Close() }
	return b, cleanup, nil
}
