package core_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yolocs/open-artifact/pkg/core"
)

func TestSentinelErrorsDistinguishable(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		core.ErrNotFound,
		core.ErrAlreadyExists,
		core.ErrDigestMismatch,
		core.ErrUnsupported,
	}

	for i, target := range sentinels {
		t.Run(target.Error(), func(t *testing.T) {
			t.Parallel()

			// A wrapped sentinel is still matched by errors.Is.
			wrapped := fmt.Errorf("context: %w", target)
			if !errors.Is(wrapped, target) {
				t.Errorf("errors.Is(wrapped, %v) = false, want true", target)
			}

			// It must not match any of the other sentinels.
			for j, other := range sentinels {
				if i == j {
					continue
				}
				if errors.Is(wrapped, other) {
					t.Errorf("errors.Is(%v, %v) = true, want false", target, other)
				}
			}
		})
	}
}
