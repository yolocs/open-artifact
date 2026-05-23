package blobstore

import (
	"context"
	"fmt"
	"io"

	"gocloud.dev/gcerrors"

	"github.com/yolocs/open-artifact/pkg/core"
)

// cacheStore is the blobstore implementation of core.Cache. It is bound to a
// single .cache/ directory (the cache root of a Store, Package, or Version) and
// stores opaque blobs named by a hashed logical key.
//
// Cache operations authorize as reads, not writes: populating the cache is part
// of serving a read (a proxy cold fill), so reader policy is sufficient — a
// subject that may read the namespace may fill its cache.
type cacheStore struct {
	store   *Store
	pathFor func(key string) string
}

func (c *cacheStore) Get(ctx context.Context, key string) (core.CacheEntry, bool, error) {
	if err := c.store.authorize(ctx, false); err != nil {
		return core.CacheEntry{}, false, err
	}
	path := c.pathFor(key)
	r, err := c.store.bNewReader(ctx, path, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return core.CacheEntry{}, false, nil
		}
		return core.CacheEntry{}, false, fmt.Errorf("blobstore: open cache %q: %w", path, mapErr(err))
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		return core.CacheEntry{}, false, fmt.Errorf("blobstore: read cache %q: %w", path, mapErr(err))
	}
	return core.CacheEntry{Body: body, ModTime: r.ModTime()}, true, nil
}

func (c *cacheStore) Put(ctx context.Context, key string, body []byte) error {
	if err := c.store.authorize(ctx, false); err != nil {
		return err
	}
	path := c.pathFor(key)
	if err := c.store.bWriteAll(ctx, path, body, nil); err != nil {
		return fmt.Errorf("blobstore: write cache %q: %w", path, mapErr(err))
	}
	return nil
}

func (c *cacheStore) Delete(ctx context.Context, key string) error {
	if err := c.store.authorize(ctx, false); err != nil {
		return err
	}
	path := c.pathFor(key)
	if err := c.store.bDelete(ctx, path); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return fmt.Errorf("blobstore: delete cache %q: %w", path, mapErr(err))
	}
	return nil
}
