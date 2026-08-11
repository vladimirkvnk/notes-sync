package notify

import (
	"context"
	"testing"
)

// TestDisabledNotifierIsNoop verifies optional notifications stay optional.
func TestDisabledNotifierIsNoop(t *testing.T) {
	if err := New(false).Conflict(context.Background(), "notes"); err != nil {
		t.Fatalf("Conflict() error = %v", err)
	}
}
