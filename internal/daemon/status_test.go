package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vladimirkvnk/notes-sync/internal/config"
)

// TestParseStatus verifies NUL-delimited porcelain parsing.
func TestParseStatus(t *testing.T) {
	raw := " M note.md\x00?? a file.txt\x00D  old.md\x00UU conflict.md\x00"
	entries, err := ParseStatus(raw)
	if err != nil {
		t.Fatalf("ParseStatus() error = %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("len(entries) = %d, want 4", len(entries))
	}
	if entries[1].Path != "a file.txt" || entries[1].Index != '?' || entries[1].Worktree != '?' {
		t.Fatalf("unexpected untracked entry: %+v", entries[1])
	}
	if !entries[2].Staged() {
		t.Fatalf("expected staged deletion: %+v", entries[2])
	}
	if !entries[3].Unmerged() {
		t.Fatalf("expected unmerged entry: %+v", entries[3])
	}
}

// TestParseStatusRejectsMalformedRecord verifies malformed input rejection.
func TestParseStatusRejectsMalformedRecord(t *testing.T) {
	if _, err := ParseStatus("M note.md\x00"); err == nil {
		t.Fatal("ParseStatus() unexpectedly accepted malformed data")
	}
}

// TestPathAllowed verifies extension and hidden-path filtering.
func TestPathAllowed(t *testing.T) {
	repo := config.Repository{Extensions: []string{".md", ".txt"}}
	tests := map[string]bool{
		"note.md":             true,
		"dir/note.txt":        true,
		"dir/note.MD":         false,
		"image.png":           false,
		".hidden.md":          false,
		"dir/.hidden/note.md": false,
		".git/note.md":        false,
		"CONFLICTS":           false,
	}
	for path, want := range tests {
		if got := pathAllowed(repo, path); got != want {
			t.Errorf("pathAllowed(%q) = %v, want %v", path, got, want)
		}
	}

	repo.IncludeHidden = true
	if !pathAllowed(repo, ".hidden.md") {
		t.Error("include_hidden did not include a hidden file")
	}
	if pathAllowed(repo, ".git/note.md") {
		t.Error(".git must always be excluded")
	}
}

// TestSplitStatusFindsStagedOutsideAndConflicts verifies status classification.
func TestSplitStatusFindsStagedOutsideAndConflicts(t *testing.T) {
	repo := config.Repository{Extensions: []string{".md"}}
	entries := []StatusEntry{
		{Index: ' ', Worktree: 'M', Path: "note.md"},
		{Index: 'A', Worktree: ' ', Path: "image.png"},
		{Index: 'U', Worktree: 'U', Path: "conflict.md"},
	}
	candidates, unmerged, stagedOutside := splitStatus(repo, entries)
	if len(candidates) != 2 || len(unmerged) != 1 {
		t.Fatalf("unexpected split: candidates=%v unmerged=%v", candidates, unmerged)
	}
	if len(stagedOutside) != 1 || stagedOutside[0] != "image.png" {
		t.Fatalf("stagedOutside = %v", stagedOutside)
	}
}

// TestFingerprintTracksContentDeletionAndSymlink verifies dirty signatures.
func TestFingerprintTracksContentDeletionAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.md")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := []StatusEntry{{Index: ' ', Worktree: 'M', Path: "note.md"}}
	first, err := fingerprint(dir, entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := fingerprint(dir, entries)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("fingerprint did not change with file contents")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	deleted, err := fingerprint(dir, entries)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == second {
		t.Fatal("fingerprint did not change after deletion")
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("target contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	linkEntries := []StatusEntry{{Index: '?', Worktree: '?', Path: "link.md"}}
	linkFirst, err := fingerprint(dir, linkEntries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("another-target", link); err != nil {
		t.Fatal(err)
	}
	linkSecond, err := fingerprint(dir, linkEntries)
	if err != nil {
		t.Fatal(err)
	}
	if linkFirst == linkSecond {
		t.Fatal("fingerprint did not change with symlink target")
	}
}
