//go:build darwin || linux

package lockfile

import (
	"path/filepath"
	"testing"
)

// TestAcquireIsExclusiveAndReleasedOnClose verifies process lock semantics.
func TestAcquireIsExclusiveAndReleasedOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes-sync.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("second Acquire() unexpectedly succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire() after Close() failed: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
