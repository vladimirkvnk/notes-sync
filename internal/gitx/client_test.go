package gitx

import (
	"slices"
	"testing"
)

// TestEnvironmentWithOverridesExistingValues verifies duplicate removal.
func TestEnvironmentWithOverridesExistingValues(t *testing.T) {
	got := environmentWith([]string{"PATH=/bin", "LC_ALL=ru_RU", "GIT_TERMINAL_PROMPT=1"}, map[string]string{
		"LC_ALL":              "C",
		"GIT_TERMINAL_PROMPT": "0",
	})
	for _, want := range []string{"PATH=/bin", "LC_ALL=C", "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(got, want) {
			t.Errorf("environment missing %q: %v", want, got)
		}
	}
	for _, unwanted := range []string{"LC_ALL=ru_RU", "GIT_TERMINAL_PROMPT=1"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("environment retained %q: %v", unwanted, got)
		}
	}
}
