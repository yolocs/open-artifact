package blobstore

import (
	"context"
	"fmt"
	"strings"
)

// Each dist-tag is stored as its own object at .tags/<tag>, whose content is
// the target version string. SetTag is therefore a single, independent write:
// there is no shared .tags file to read-modify-write, so concurrent SetTag of
// distinct tags never contend and no in-process mutex is needed. Concurrent
// writers of the same tag resolve to last-write-wins, which is the natural and
// only sensible semantics for a single alias.

// readTagTarget reads the target version recorded for a single dist-tag,
// mapping a missing object to ErrNotFound via mapErr.
func (s *Store) readTagTarget(ctx context.Context, pkg, tag string) (string, error) {
	raw, err := s.bReadAll(ctx, tagPath(s.scope, pkg, tag))
	if err != nil {
		return "", fmt.Errorf("blobstore: read tag %q/%q: %w", pkg, tag, mapErr(err))
	}
	return strings.TrimSpace(string(raw)), nil
}

// writeTagTarget points a dist-tag at target by writing the single tag object.
func (s *Store) writeTagTarget(ctx context.Context, pkg, tag, target string) error {
	if err := s.bWriteAll(ctx, tagPath(s.scope, pkg, tag), []byte(target), nil); err != nil {
		return fmt.Errorf("blobstore: write tag %q/%q: %w", pkg, tag, mapErr(err))
	}
	return nil
}
