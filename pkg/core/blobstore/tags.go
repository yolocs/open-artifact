package blobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"gocloud.dev/gcerrors"
)

// The .tags object is a flat JSON alias map, tag name -> version string, e.g.
// {"latest":"2.31.0","beta":"3.0.0rc1"}. SetTag is a read-modify-write guarded
// by an in-process mutex keyed on the package's .tags path. This serializes
// concurrent writers within a single replica; across replicas the last writer
// still wins (a lost update is possible). A compare-and-swap on the backing
// object is the planned follow-up — documented here as a known v1 limitation.

// readTagMap reads and parses a package's .tags object. A missing object is
// not an error: it yields an empty map.
func (s *Store) readTagMap(ctx context.Context, pkg string) (map[string]string, error) {
	raw, err := s.bucket.ReadAll(ctx, packageTagsPath(s.scope, pkg))
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("blobstore: read tags for %q: %w", pkg, mapErr(err))
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("blobstore: decode tags for %q: %w", pkg, err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// writeTagMap serializes and writes a package's .tags object.
func (s *Store) writeTagMap(ctx context.Context, pkg string, m map[string]string) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("blobstore: encode tags for %q: %w", pkg, err)
	}
	if err := s.bucket.WriteAll(ctx, packageTagsPath(s.scope, pkg), raw, nil); err != nil {
		return fmt.Errorf("blobstore: write tags for %q: %w", pkg, mapErr(err))
	}
	return nil
}

// tagMutex returns the process-local mutex guarding a package's .tags object,
// creating it on first use.
func (s *Store) tagMutex(key string) *sync.Mutex {
	v, _ := s.tagLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}
