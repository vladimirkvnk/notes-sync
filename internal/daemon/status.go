package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/vladimirkvnk/notes-sync/internal/config"
)

// StatusEntry is one record from git status --porcelain=v1 -z --no-renames.
type StatusEntry struct {
	Index    byte
	Worktree byte
	Path     string
}

// Staged reports whether the index contains the path.
func (e StatusEntry) Staged() bool {
	return e.Index != ' ' && e.Index != '?'
}

// Unmerged reports whether the status code represents a conflict.
func (e StatusEntry) Unmerged() bool {
	status := string([]byte{e.Index, e.Worktree})
	switch status {
	case "DD", "AU", "UD", "UA", "DU", "AA", "UU":
		return true
	default:
		return false
	}
}

// ParseStatus parses the stable, NUL-delimited porcelain format. Rename
// detection is disabled by the caller, so every record contains one path.
func ParseStatus(raw string) ([]StatusEntry, error) {
	if raw == "" {
		return nil, nil
	}
	records := strings.Split(raw, "\x00")
	entries := make([]StatusEntry, 0, len(records))
	for i, record := range records {
		if record == "" && i == len(records)-1 {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("invalid Git status record %d", i)
		}
		entries = append(entries, StatusEntry{
			Index:    record[0],
			Worktree: record[1],
			Path:     record[3:],
		})
	}
	return entries, nil
}

// pathAllowed applies repository path and extension filters.
func pathAllowed(repo config.Repository, gitPath string) bool {
	gitPath = strings.TrimPrefix(gitPath, "./")
	if gitPath == "" || gitPath == "CONFLICTS" {
		return false
	}
	for part := range strings.SplitSeq(gitPath, "/") {
		if part == ".git" {
			return false
		}
		if !repo.IncludeHidden && strings.HasPrefix(part, ".") {
			return false
		}
	}
	ext := path.Ext(gitPath)
	return slices.Contains(repo.Extensions, ext)
}

// splitStatus classifies status records needed by the worker.
func splitStatus(repo config.Repository, entries []StatusEntry) (candidates, unmerged []StatusEntry, stagedOutside []string) {
	for _, entry := range entries {
		if entry.Unmerged() {
			unmerged = append(unmerged, entry)
		}
		if pathAllowed(repo, entry.Path) {
			candidates = append(candidates, entry)
			continue
		}
		if entry.Staged() {
			stagedOutside = append(stagedOutside, entry.Path)
		}
	}
	slices.SortFunc(candidates, func(a, b StatusEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(unmerged, func(a, b StatusEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(stagedOutside)
	return candidates, unmerged, stagedOutside
}

// errUnstableFile asks the worker to retry a racing file on the next scan.
var errUnstableFile = errors.New("file changed while fingerprinting")

// fingerprint hashes the status and current contents of dirty paths.
func fingerprint(root string, entries []StatusEntry) (string, error) {
	h := sha256.New()
	for _, entry := range entries {
		writeHashPart(h, string([]byte{entry.Index, entry.Worktree}))
		writeHashPart(h, entry.Path)

		filePath := filepath.Join(root, filepath.FromSlash(entry.Path))
		before, err := os.Lstat(filePath)
		if errors.Is(err, os.ErrNotExist) {
			writeHashPart(h, "deleted")
			continue
		}
		if err != nil {
			return "", fmt.Errorf("stat %q: %w", entry.Path, err)
		}
		writeHashPart(h, before.Mode().String())

		if before.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filePath)
			if err != nil {
				return "", fmt.Errorf("read symlink %q: %w", entry.Path, err)
			}
			writeHashPart(h, target)
			continue
		}
		if !before.Mode().IsRegular() {
			writeHashPart(h, "non-regular")
			continue
		}

		file, err := os.Open(filePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", errUnstableFile
			}
			return "", fmt.Errorf("open %q: %w", entry.Path, err)
		}
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash %q: %w", entry.Path, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close %q: %w", entry.Path, closeErr)
		}

		after, err := os.Lstat(filePath)
		if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
			return "", errUnstableFile
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeHashPart writes an unambiguous length-prefixed value.
func writeHashPart(h hash.Hash, value string) {
	_, _ = fmt.Fprintf(h, "%d:", len(value))
	_, _ = io.WriteString(h, value)
}
