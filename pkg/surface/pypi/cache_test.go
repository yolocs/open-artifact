package pypi

import (
	"testing"
	"time"
)

func TestProjectCacheDoesNotStoreStaleLoadAfterInvalidation(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0)
	cache := newProjectCache(time.Minute, func() time.Time { return now })
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan ProjectPage)

	go func() {
		page, err := cache.get("team-a", "demo", func() (ProjectPage, error) {
			close(started)
			<-release
			return ProjectPage{Name: "stale"}, nil
		})
		if err != nil {
			t.Errorf("first get: %v", err)
		}
		done <- page
	}()

	<-started
	cache.invalidate("team-a", "demo")
	close(release)
	if got := <-done; got.Name != "stale" {
		t.Fatalf("first get returned %q, want stale", got.Name)
	}

	loads := 0
	got, err := cache.get("team-a", "demo", func() (ProjectPage, error) {
		loads++
		return ProjectPage{Name: "fresh"}, nil
	})
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if got.Name != "fresh" || loads != 1 {
		t.Fatalf("second get = (%q, loads %d), want fresh load", got.Name, loads)
	}
}
