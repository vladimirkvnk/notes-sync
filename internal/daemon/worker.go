package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/vladimirkvnk/notes-sync/internal/config"
	"github.com/vladimirkvnk/notes-sync/internal/gitx"
	notification "github.com/vladimirkvnk/notes-sync/internal/notify"
)

// repositoryState describes the user-visible worker condition.
type repositoryState string

const (
	// stateHealthy permits local and remote operations.
	stateHealthy repositoryState = "healthy"
	// stateDegraded permits local commits while remote access is unavailable.
	stateDegraded repositoryState = "degraded"
	// stateBlocked prevents unsafe repository mutations.
	stateBlocked repositoryState = "blocked"
	// stateConflict waits for explicit manual resolution.
	stateConflict repositoryState = "conflict"
)

// worker serializes all operations for one repository.
type worker struct {
	repo         config.Repository
	info         gitx.RepositoryInfo
	git          *gitx.Client
	notifier     Notifier
	logger       *slog.Logger
	scanInterval time.Duration
	debounce     time.Duration
	gitTimeout   time.Duration

	lastSignature    string
	stableSince      time.Time
	syncDue          bool
	conflict         bool
	conflictNotified bool
	blockedReason    string
	degradedReason   string
	state            repositoryState
}

// newWorker constructs one repository state machine.
func newWorker(
	cfg config.Config,
	repo config.Repository,
	info gitx.RepositoryInfo,
	git *gitx.Client,
	notifier Notifier,
	logger *slog.Logger,
) *worker {
	return &worker{
		repo:         repo,
		info:         info,
		git:          git,
		notifier:     notifier,
		logger:       logger.With("repository", info.Name, "path", info.Root),
		scanInterval: cfg.ScanInterval,
		debounce:     cfg.Debounce,
		gitTimeout:   cfg.GitTimeout,
		syncDue:      true,
		state:        stateHealthy,
	}
}

// run polls and synchronizes a repository until cancellation.
func (w *worker) run(ctx context.Context) error {
	w.logger.Info("repository worker started", "branch", w.info.Branch, "upstream", w.info.Upstream)
	scanTicker := time.NewTicker(w.scanInterval)
	syncTicker := time.NewTicker(w.repo.SyncInterval)
	defer scanTicker.Stop()
	defer syncTicker.Stop()

	if err := w.cycle(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Debug("initial repository cycle failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("worker stopped: %w", ctx.Err())
		case now := <-scanTicker.C:
			if err := w.cycle(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Debug("repository cycle failed", "error", err)
			}
		case now := <-syncTicker.C:
			w.syncDue = true
			if err := w.cycle(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Debug("scheduled repository cycle failed", "error", err)
			}
		}
	}
}

// cycle performs one state check and any due work.
func (w *worker) cycle(ctx context.Context, now time.Time) error {
	paused, err := w.handleConflictState(ctx)
	if err != nil || paused {
		return err
	}

	operation, err := inProgressOperation(w.info.GitDir)
	if err != nil {
		w.setBlocked(err.Error())
		return err
	}
	if operation != "" {
		err := fmt.Errorf("git %s operation is in progress", operation)
		w.setBlocked(err.Error())
		return err
	}

	entries, err := w.status(ctx)
	if err != nil {
		w.setBlocked(err.Error())
		return err
	}
	candidates, unmerged, stagedOutside := splitStatus(w.repo, entries)
	if len(unmerged) > 0 {
		return w.enterConflict(ctx, unmerged, "unmerged Git entries detected")
	}
	if len(stagedOutside) > 0 {
		err := fmt.Errorf("staged paths outside configured extensions: %s", summarizePaths(stagedOutside))
		w.setBlocked(err.Error())
		return err
	}
	if len(candidates) > 0 {
		signature, err := fingerprint(w.info.Root, candidates)
		if err != nil {
			if errors.Is(err, errUnstableFile) {
				w.lastSignature = ""
				return nil
			}
			w.setBlocked(err.Error())
			return err
		}
		if signature != w.lastSignature {
			w.lastSignature = signature
			w.stableSince = now
			return nil
		}
		if now.Sub(w.stableSince) < w.debounce {
			return nil
		}

		wasDegraded := w.degradedReason != ""
		committed, err := w.commitCandidates(ctx)
		if err != nil {
			return err
		}
		w.lastSignature = ""
		w.stableSince = time.Time{}
		if committed && !wasDegraded {
			w.syncDue = true
		}
		if !committed {
			return nil
		}
	} else {
		w.lastSignature = ""
		w.stableSince = time.Time{}
	}

	if !w.syncDue {
		return nil
	}
	w.syncDue = false
	return w.syncRemote(ctx)
}

// handleConflictState maintains the manual conflict handshake.
func (w *worker) handleConflictState(ctx context.Context) (bool, error) {
	markerExists, err := pathExists(filepath.Join(w.info.Root, "CONFLICTS"))
	if err != nil {
		w.setBlocked(err.Error())
		return true, err
	}
	mergeExists, err := pathExists(filepath.Join(w.info.GitDir, "MERGE_HEAD"))
	if err != nil {
		w.setBlocked(err.Error())
		return true, err
	}

	if markerExists {
		if !w.conflict {
			w.conflict = true
			w.refreshState("CONFLICTS exists")
			w.logger.Error("synchronization paused by CONFLICTS marker", "marker", filepath.Join(w.info.Root, "CONFLICTS"))
			w.notifyConflict(ctx)
		}
		return true, nil
	}
	if mergeExists && !w.conflict {
		entries, statusErr := w.status(ctx)
		if statusErr != nil {
			w.setBlocked(statusErr.Error())
			return true, statusErr
		}
		_, unmerged, _ := splitStatus(w.repo, entries)
		if err := w.enterConflict(ctx, unmerged, "unfinished merge detected"); err != nil {
			return true, err
		}
		return true, nil
	}
	if !w.conflict {
		return false, nil
	}

	entries, err := w.status(ctx)
	if err != nil {
		return true, err
	}
	_, unmerged, _ := splitStatus(w.repo, entries)
	if len(unmerged) > 0 {
		if err := w.writeConflictFile(unmerged, "conflicts are still unresolved"); err != nil {
			w.setBlocked(err.Error())
			return true, err
		}
		return true, nil
	}
	if mergeExists {
		if _, err := w.gitRun(ctx, "commit", "--no-edit"); err != nil {
			writeErr := w.writeConflictFile(nil, "Git could not complete the merge commit")
			if writeErr != nil {
				return true, errors.Join(err, writeErr)
			}
			return true, err
		}
		w.logger.Info("manual conflict resolution committed")
	}

	w.conflict = false
	w.conflictNotified = false
	w.clearBlocked()
	w.syncDue = true
	w.refreshState("conflict resolved")
	return false, nil
}

// commitCandidates commits only accepted paths.
func (w *worker) commitCandidates(ctx context.Context) (bool, error) {
	if err := w.runtimeWiring(ctx); err != nil {
		w.setBlocked(err.Error())
		return false, err
	}
	entries, err := w.status(ctx)
	if err != nil {
		w.setBlocked(err.Error())
		return false, err
	}
	candidates, unmerged, stagedOutside := splitStatus(w.repo, entries)
	if len(unmerged) > 0 {
		return false, w.enterConflict(ctx, unmerged, "unmerged Git entries detected")
	}
	if len(stagedOutside) > 0 {
		err := fmt.Errorf("staged paths outside configured extensions: %s", summarizePaths(stagedOutside))
		w.setBlocked(err.Error())
		return false, err
	}
	if len(candidates) == 0 {
		return false, nil
	}

	paths := uniquePaths(candidates)
	args := append([]string{"add", "-A", "--"}, paths...)
	if _, err := w.gitRun(ctx, args...); err != nil {
		w.setBlocked(err.Error())
		return false, err
	}

	entries, err = w.status(ctx)
	if err != nil {
		w.setBlocked(err.Error())
		return false, err
	}
	_, unmerged, stagedOutside = splitStatus(w.repo, entries)
	if len(unmerged) > 0 {
		return false, w.enterConflict(ctx, unmerged, "unmerged Git entries detected after staging")
	}
	if len(stagedOutside) > 0 {
		err := fmt.Errorf("staged paths outside configured extensions: %s", summarizePaths(stagedOutside))
		w.setBlocked(err.Error())
		return false, err
	}
	staged := false
	for _, entry := range entries {
		if entry.Staged() {
			staged = true
			break
		}
	}
	if !staged {
		return false, nil
	}

	message := fmt.Sprintf("%s: sync %s", w.info.Name, time.Now().Format(time.RFC3339))
	if _, err := w.gitRun(ctx, "commit", "-m", message); err != nil {
		w.setBlocked(err.Error())
		return false, err
	}
	oid, err := w.gitRun(ctx, "rev-parse", "HEAD")
	if err != nil {
		w.setBlocked(err.Error())
		return true, err
	}
	w.clearBlocked()
	w.logger.Info("local changes committed", "commit", gitx.ShortOID(oid), "files", len(paths))
	return true, nil
}

// syncRemote fetches, merges, and pushes the upstream.
func (w *worker) syncRemote(ctx context.Context) error {
	if err := w.runtimeWiring(ctx); err != nil {
		w.setBlocked(err.Error())
		return err
	}
	entries, err := w.status(ctx)
	if err != nil {
		w.setBlocked(err.Error())
		return err
	}
	candidates, unmerged, stagedOutside := splitStatus(w.repo, entries)
	if len(unmerged) > 0 {
		return w.enterConflict(ctx, unmerged, "unmerged Git entries detected before synchronization")
	}
	if len(stagedOutside) > 0 {
		err := fmt.Errorf("staged paths outside configured extensions: %s", summarizePaths(stagedOutside))
		w.setBlocked(err.Error())
		return err
	}
	if len(candidates) > 0 {
		w.syncDue = true
		return nil
	}

	headBefore, _ := w.gitRun(ctx, "rev-parse", "HEAD")
	if _, err := w.gitRun(ctx, "fetch", w.info.Remote); err != nil {
		w.setDegraded(err.Error())
		return err
	}
	if _, err := w.gitRun(ctx, "merge", "--no-edit", w.info.Upstream); err != nil {
		return w.handleMergeFailure(ctx, err)
	}

	headAfter, err := w.gitRun(ctx, "rev-parse", "HEAD")
	if err != nil {
		w.setBlocked(err.Error())
		return err
	}
	upstreamOID, err := w.gitRun(ctx, "rev-parse", "@{upstream}")
	if err != nil {
		w.setBlocked(err.Error())
		return err
	}
	pushed := false
	if strings.TrimSpace(headAfter) != strings.TrimSpace(upstreamOID) {
		if _, err := w.gitRun(ctx, "push", w.info.Remote, "HEAD:"+w.info.MergeRef); err != nil {
			if retryErr := w.retryPushAfterRace(ctx, strings.TrimSpace(upstreamOID)); retryErr != nil {
				if !w.conflict {
					w.setDegraded(retryErr.Error())
				}
				return retryErr
			}
		}
		pushed = true
	}

	pulled := strings.TrimSpace(headBefore) != strings.TrimSpace(headAfter)
	w.clearBlocked()
	w.clearDegraded()
	if pulled || pushed {
		w.logger.Info("repository synchronized", "pulled", pulled, "pushed", pushed)
	}
	return nil
}

// retryPushAfterRace retries once when the upstream moved.
func (w *worker) retryPushAfterRace(ctx context.Context, previousUpstream string) error {
	if _, err := w.gitRun(ctx, "fetch", w.info.Remote); err != nil {
		return err
	}
	upstreamOID, err := w.gitRun(ctx, "rev-parse", "@{upstream}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(upstreamOID) == previousUpstream {
		return errors.New("git push failed and upstream did not change")
	}
	if _, err := w.gitRun(ctx, "merge", "--no-edit", w.info.Upstream); err != nil {
		return w.handleMergeFailure(ctx, err)
	}
	if _, err := w.gitRun(ctx, "push", w.info.Remote, "HEAD:"+w.info.MergeRef); err != nil {
		return err
	}
	return nil
}

// handleMergeFailure distinguishes conflicts from operational failures.
func (w *worker) handleMergeFailure(ctx context.Context, mergeErr error) error {
	entries, statusErr := w.status(ctx)
	if statusErr != nil {
		w.setBlocked(statusErr.Error())
		return errors.Join(mergeErr, statusErr)
	}
	_, unmerged, _ := splitStatus(w.repo, entries)
	mergeExists, statErr := pathExists(filepath.Join(w.info.GitDir, "MERGE_HEAD"))
	if statErr != nil {
		w.setBlocked(statErr.Error())
		return errors.Join(mergeErr, statErr)
	}
	if len(unmerged) > 0 || mergeExists {
		if err := w.enterConflict(ctx, unmerged, "merge with upstream failed"); err != nil {
			return errors.Join(mergeErr, err)
		}
		return mergeErr
	}
	w.setBlocked(mergeErr.Error())
	return mergeErr
}

// enterConflict creates the durable pause marker and notification.
func (w *worker) enterConflict(ctx context.Context, entries []StatusEntry, reason string) error {
	if err := w.writeConflictFile(entries, reason); err != nil {
		w.setBlocked(err.Error())
		return err
	}
	w.conflict = true
	w.blockedReason = ""
	w.degradedReason = ""
	w.refreshState(reason)
	w.logger.Error("synchronization paused by conflict", "conflicts", len(entries), "marker", filepath.Join(w.info.Root, "CONFLICTS"))
	w.notifyConflict(ctx)
	return nil
}

// notifyConflict sends at most one notification per conflict.
func (w *worker) notifyConflict(ctx context.Context) {
	if !w.conflictNotified {
		w.conflictNotified = true
		notifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := w.notifier.Conflict(notifyCtx, w.info.Name)
		cancel()
		if err != nil && !errors.Is(err, notification.ErrUnavailable) {
			w.logger.Debug("desktop conflict notification failed", "error", err)
		}
	}
}

// writeConflictFile atomically writes recovery instructions.
func (w *worker) writeConflictFile(entries []StatusEntry, reason string) error {
	marker := filepath.Join(w.info.Root, "CONFLICTS")
	if exists, err := pathExists(marker); err != nil {
		return err
	} else if exists {
		return nil
	}

	var body strings.Builder
	fmt.Fprint(&body, "notes-sync paused synchronization for this repository.\n\n")
	fmt.Fprintf(&body, "Detected: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&body, "Repository: %s\n", w.info.Root)
	fmt.Fprintf(&body, "Upstream: %s\n", w.info.Upstream)
	fmt.Fprintf(&body, "Reason: %s\n", reason)
	if len(entries) > 0 {
		body.WriteString("\nConflicting paths:\n")
		for _, entry := range entries {
			fmt.Fprintf(&body, "- %q\n", entry.Path)
		}
	}
	body.WriteString("\nResolution steps:\n")
	body.WriteString("1. Run `git status` in this repository.\n")
	body.WriteString("2. Edit every conflicted file and remove conflict markers.\n")
	body.WriteString("3. Stage only the resolved files with `git add -- <files>`.\n")
	body.WriteString("4. Delete this CONFLICTS file.\n\n")
	body.WriteString("notes-sync will verify the index, complete the merge commit, and push.\n")

	temp, err := os.CreateTemp(w.info.Root, ".notes-sync-conflicts-*")
	if err != nil {
		return fmt.Errorf("create conflict marker: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set conflict marker permissions: %w", err)
	}
	if _, err := temp.WriteString(body.String()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write conflict marker: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync conflict marker: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close conflict marker: %w", err)
	}
	if err := os.Rename(tempName, marker); err != nil {
		return fmt.Errorf("install conflict marker: %w", err)
	}
	return nil
}

// status returns machine-readable Git status entries.
func (w *worker) status(ctx context.Context) ([]StatusEntry, error) {
	raw, err := w.gitRun(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, err
	}
	return ParseStatus(raw)
}

// runtimeWiring verifies branch ownership.
func (w *worker) runtimeWiring(ctx context.Context) error {
	commandCtx, cancel := context.WithTimeout(ctx, w.gitTimeout)
	defer cancel()
	if err := gitx.RuntimeWiringMatches(commandCtx, w.git, w.info); err != nil {
		return fmt.Errorf("validate Git wiring: %w", err)
	}
	return nil
}

// gitRun executes one bounded Git command.
func (w *worker) gitRun(ctx context.Context, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, w.gitTimeout)
	defer cancel()
	output, err := w.git.Run(commandCtx, w.info.Root, args...)
	if err != nil {
		return output, fmt.Errorf("run Git command: %w", err)
	}
	return output, nil
}

// setBlocked records a safety failure.
func (w *worker) setBlocked(reason string) {
	w.blockedReason = reason
	w.refreshState(reason)
}

// clearBlocked clears a verified safety failure.
func (w *worker) clearBlocked() {
	if w.blockedReason == "" {
		return
	}
	w.blockedReason = ""
	w.refreshState("blocking condition cleared")
}

// setDegraded records a remote failure.
func (w *worker) setDegraded(reason string) {
	w.degradedReason = reason
	w.refreshState(reason)
}

// clearDegraded records remote recovery.
func (w *worker) clearDegraded() {
	if w.degradedReason == "" {
		return
	}
	w.degradedReason = ""
	w.refreshState("remote synchronization recovered")
}

// refreshState emits only actual state transitions.
func (w *worker) refreshState(reason string) {
	next := stateHealthy
	switch {
	case w.conflict:
		next = stateConflict
	case w.blockedReason != "":
		next = stateBlocked
	case w.degradedReason != "":
		next = stateDegraded
	default:
	}
	if next == w.state {
		return
	}
	w.state = next
	if next == stateConflict {
		// enterConflict and the pre-existing-marker path emit one richer error
		// containing the marker location.
		return
	}
	if next == stateHealthy {
		w.logger.Info("repository recovered", "state", next, "reason", reason)
		return
	}
	w.logger.Warn("repository state changed", "state", next, "reason", reason)
}

// inProgressOperation finds unsafe Git sequencer state.
func inProgressOperation(gitDir string) (string, error) {
	checks := []struct {
		name string
		path string
	}{
		{"merge", "MERGE_HEAD"},
		{"rebase", "rebase-merge"},
		{"rebase", "rebase-apply"},
		{"cherry-pick", "CHERRY_PICK_HEAD"},
		{"revert", "REVERT_HEAD"},
		{"bisect", "BISECT_START"},
	}
	for _, check := range checks {
		exists, err := pathExists(filepath.Join(gitDir, check.path))
		if err != nil {
			return "", err
		}
		if exists {
			return check.name, nil
		}
	}
	return "", nil
}

// pathExists checks a path without following symlinks.
func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect %q: %w", path, err)
}

// uniquePaths returns sorted unique status paths.
func uniquePaths(entries []StatusEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// summarizePaths bounds path detail in service logs.
func summarizePaths(paths []string) string {
	const limit = 3
	quoted := make([]string, 0, min(len(paths), limit))
	for _, path := range paths[:min(len(paths), limit)] {
		quoted = append(quoted, fmt.Sprintf("%q", path))
	}
	if len(paths) > limit {
		quoted = append(quoted, fmt.Sprintf("and %d more", len(paths)-limit))
	}
	return strings.Join(quoted, ", ")
}
