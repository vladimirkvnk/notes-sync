package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vladimirkvnk/notes-sync/internal/config"
	"github.com/vladimirkvnk/notes-sync/internal/gitx"
)

// testRepository contains a bare remote and two configured clones.
type testRepository struct {
	remote string
	local  string
	peer   string
	branch string
}

// TestWorkerCommitsOnlyAllowedFilesAndPushes verifies local filtering and push.
func TestWorkerCommitsOnlyAllowedFilesAndPushes(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)

	writeTestFile(t, filepath.Join(fixture.local, "note.md"), "hello\n")
	writeTestFile(t, filepath.Join(fixture.local, "image.png"), "not managed\n")
	writeTestFile(t, filepath.Join(fixture.local, ".hidden.md"), "hidden\n")
	writeTestFile(t, filepath.Join(fixture.local, ".private", "secret.txt"), "hidden directory\n")

	cycleCommit(t, w)
	runGit(t, fixture.peer, "pull", "--ff-only")
	if got := readTestFile(t, filepath.Join(fixture.peer, "note.md")); got != "hello\n" {
		t.Fatalf("peer note contents = %q", got)
	}
	tracked := runGit(t, fixture.local, "ls-files")
	for _, excluded := range []string{"image.png", ".hidden.md", ".private/secret.txt"} {
		if strings.Contains(tracked, excluded) {
			t.Errorf("excluded path %q was committed; tracked files:\n%s", excluded, tracked)
		}
	}
	status := runGit(t, fixture.local, "status", "--porcelain")
	if !strings.Contains(status, "image.png") || !strings.Contains(status, ".hidden.md") {
		t.Fatalf("excluded files should remain untracked; status:\n%s", status)
	}
	message := strings.TrimSpace(runGit(t, fixture.local, "log", "-1", "--format=%s"))
	if !strings.HasPrefix(message, filepath.Base(fixture.local)+": sync ") {
		t.Fatalf("commit message = %q", message)
	}
}

// TestWorkerCommitsDeletionRenameAndUnusualFilename verifies path-safe staging.
func TestWorkerCommitsDeletionRenameAndUnusualFilename(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	if err := os.Rename(filepath.Join(fixture.local, "base.md"), filepath.Join(fixture.local, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	strange := "line\nbreak.md"
	writeTestFile(t, filepath.Join(fixture.local, strange), "unusual name\n")

	cycleCommit(t, w)
	runGit(t, fixture.peer, "pull", "--ff-only")
	if _, err := os.Stat(filepath.Join(fixture.peer, "base.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted source still exists on peer: %v", err)
	}
	if got := readTestFile(t, filepath.Join(fixture.peer, "renamed.txt")); got != "base\n" {
		t.Fatalf("renamed contents = %q", got)
	}
	if got := readTestFile(t, filepath.Join(fixture.peer, strange)); got != "unusual name\n" {
		t.Fatalf("unusual filename contents = %q", got)
	}
}

// TestWorkerCommitsPreStagedRename verifies synchronization after git mv.
func TestWorkerCommitsPreStagedRename(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	runGit(t, fixture.local, "mv", "base.md", "renamed.md")
	writeTestFile(t, filepath.Join(fixture.local, "renamed.md"), "renamed and edited\n")

	cycleCommit(t, w)
	runGit(t, fixture.peer, "pull", "--ff-only")
	if _, err := os.Stat(filepath.Join(fixture.peer, "base.md")); !os.IsNotExist(err) {
		t.Fatalf("renamed source still exists on peer: %v", err)
	}
	if got := readTestFile(t, filepath.Join(fixture.peer, "renamed.md")); got != "renamed and edited\n" {
		t.Fatalf("renamed contents = %q", got)
	}
}

// TestWorkerCommitsPreStagedDeletion verifies synchronization after git rm.
func TestWorkerCommitsPreStagedDeletion(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	runGit(t, fixture.local, "rm", "base.md")

	cycleCommit(t, w)
	runGit(t, fixture.peer, "pull", "--ff-only")
	if _, err := os.Stat(filepath.Join(fixture.peer, "base.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still exists on peer: %v", err)
	}
}

// TestWorkerRespectsGitignore verifies ignored files stay unmanaged.
func TestWorkerRespectsGitignore(t *testing.T) {
	fixture := newTestRepository(t)
	writeTestFile(t, filepath.Join(fixture.local, ".gitignore"), "ignored.md\n")
	runGit(t, fixture.local, "add", ".gitignore")
	runGit(t, fixture.local, "commit", "-m", "ignore generated note")
	runGit(t, fixture.local, "push")
	w, _ := newTestWorker(t, fixture.local)

	writeTestFile(t, filepath.Join(fixture.local, "ignored.md"), "ignored\n")
	writeTestFile(t, filepath.Join(fixture.local, "kept.md"), "kept\n")
	cycleCommit(t, w)
	if tracked := runGit(t, fixture.local, "ls-files", "--", "ignored.md"); strings.TrimSpace(tracked) != "" {
		t.Fatalf("gitignored file was tracked: %q", tracked)
	}
}

// TestWorkerCanIncludeHiddenFilesExplicitly verifies the hidden override.
func TestWorkerCanIncludeHiddenFilesExplicitly(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	w.repo.IncludeHidden = true
	writeTestFile(t, filepath.Join(fixture.local, ".hidden.md"), "included\n")

	cycleCommit(t, w)
	runGit(t, fixture.peer, "pull", "--ff-only")
	if got := readTestFile(t, filepath.Join(fixture.peer, ".hidden.md")); got != "included\n" {
		t.Fatalf("hidden file contents = %q", got)
	}
}

// TestWorkerDebouncesUntilContentsAreStable verifies content-based settling.
func TestWorkerDebouncesUntilContentsAreStable(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	w.syncDue = false
	baseHead := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD"))
	start := time.Unix(1_700_000_000, 0)

	writeTestFile(t, filepath.Join(fixture.local, "note.md"), "one\n")
	if err := w.cycle(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fixture.local, "note.md"), "two\n")
	if err := w.cycle(context.Background(), start.Add(w.debounce)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD")); got != baseHead {
		t.Fatal("worker committed before the changed contents settled")
	}
	if err := w.cycle(context.Background(), start.Add(2*w.debounce)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD")); got == baseHead {
		t.Fatal("worker did not commit stable contents")
	}
}

// TestWorkerPullsRemoteChangesOnScheduledCycle verifies periodic fetch and merge.
func TestWorkerPullsRemoteChangesOnScheduledCycle(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	writeTestFile(t, filepath.Join(fixture.peer, "remote.md"), "from peer\n")
	runGit(t, fixture.peer, "add", "remote.md")
	runGit(t, fixture.peer, "commit", "-m", "peer change")
	runGit(t, fixture.peer, "push")

	if err := w.cycle(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(fixture.local, "remote.md")); got != "from peer\n" {
		t.Fatalf("pulled contents = %q", got)
	}
}

// TestWorkerMergesDivergentNonConflictingChanges verifies merge commits.
func TestWorkerMergesDivergentNonConflictingChanges(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	start := time.Unix(1_700_000_000, 0)

	writeTestFile(t, filepath.Join(fixture.local, "local.md"), "local\n")
	if err := w.cycle(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fixture.peer, "remote.md"), "remote\n")
	runGit(t, fixture.peer, "add", "remote.md")
	runGit(t, fixture.peer, "commit", "-m", "remote divergence")
	runGit(t, fixture.peer, "push")

	if err := w.cycle(context.Background(), start.Add(w.debounce)); err != nil {
		t.Fatal(err)
	}
	parents := strings.Fields(runGit(t, fixture.local, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("expected a merge commit with two parents, got %v", parents)
	}
	runGit(t, fixture.peer, "pull", "--ff-only")
	if readTestFile(t, filepath.Join(fixture.peer, "local.md")) != "local\n" {
		t.Fatal("local side of merge was not pushed")
	}
}

// TestWorkerCreatesConflictMarkerAndResumesAfterManualResolution verifies recovery.
func TestWorkerCreatesConflictMarkerAndResumesAfterManualResolution(t *testing.T) {
	fixture := newTestRepository(t)
	notifier := &recordingNotifier{}
	w, _ := newTestWorkerWithNotifier(t, fixture.local, notifier)
	start := time.Unix(1_700_000_000, 0)

	writeTestFile(t, filepath.Join(fixture.local, "base.md"), "local version\n")
	if err := w.cycle(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fixture.peer, "base.md"), "remote version\n")
	runGit(t, fixture.peer, "add", "base.md")
	runGit(t, fixture.peer, "commit", "-m", "remote conflict")
	runGit(t, fixture.peer, "push")

	if err := w.cycle(context.Background(), start.Add(w.debounce)); err == nil {
		t.Fatal("conflicting cycle unexpectedly succeeded")
	}
	marker := filepath.Join(fixture.local, "CONFLICTS")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("CONFLICTS marker was not created: %v", err)
	}
	if notifier.count() != 1 {
		t.Fatalf("notification count = %d, want 1", notifier.count())
	}
	conflictHead := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD"))
	writeTestFile(t, filepath.Join(fixture.local, "paused.md"), "must remain uncommitted\n")
	if err := w.cycle(context.Background(), start.Add(w.debounce+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD")); got != conflictHead {
		t.Fatal("worker committed while CONFLICTS existed")
	}
	if err := os.Remove(filepath.Join(fixture.local, "paused.md")); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := w.cycle(context.Background(), start.Add(2*w.debounce)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("prematurely deleted marker was not recreated: %v", err)
	}

	writeTestFile(t, filepath.Join(fixture.local, "base.md"), "resolved version\n")
	runGit(t, fixture.local, "add", "--", "base.md")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := w.cycle(context.Background(), start.Add(3*w.debounce)); err != nil {
		t.Fatal(err)
	}
	if w.state != stateHealthy {
		t.Fatalf("state after resolution = %s", w.state)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still exists after resolution: %v", err)
	}
	runGit(t, fixture.peer, "pull", "--ff-only")
	if got := readTestFile(t, filepath.Join(fixture.peer, "base.md")); got != "resolved version\n" {
		t.Fatalf("resolved contents = %q", got)
	}
}

// TestWorkerKeepsLocalCommitWhenRemoteIsUnavailable verifies offline durability.
func TestWorkerKeepsLocalCommitWhenRemoteIsUnavailable(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	start := time.Unix(1_700_000_000, 0)
	writeTestFile(t, filepath.Join(fixture.local, "offline.md"), "offline\n")
	if err := w.cycle(context.Background(), start); err != nil {
		t.Fatal(err)
	}

	offlinePath := fixture.remote + ".offline"
	if err := os.Rename(fixture.remote, offlinePath); err != nil {
		t.Fatal(err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = os.Rename(offlinePath, fixture.remote)
		}
	}()
	if err := w.cycle(context.Background(), start.Add(w.debounce)); err == nil {
		t.Fatal("sync unexpectedly succeeded with unavailable remote")
	}
	if w.state != stateDegraded {
		t.Fatalf("state = %s, want degraded", w.state)
	}
	if status := runGit(t, fixture.local, "status", "--porcelain", "--", "offline.md"); strings.TrimSpace(status) != "" {
		t.Fatalf("local change was not safely committed: %s", status)
	}

	if err := os.Rename(offlinePath, fixture.remote); err != nil {
		t.Fatal(err)
	}
	restored = true
	w.syncDue = true
	if err := w.cycle(context.Background(), start.Add(2*w.debounce)); err != nil {
		t.Fatal(err)
	}
	if w.state != stateHealthy {
		t.Fatalf("state after remote recovery = %s", w.state)
	}
	runGit(t, fixture.peer, "pull", "--ff-only")
	if readTestFile(t, filepath.Join(fixture.peer, "offline.md")) != "offline\n" {
		t.Fatal("offline commit was not eventually pushed")
	}
}

// TestWorkerBlocksOnStagedOutsidePath verifies index ownership safety.
func TestWorkerBlocksOnStagedOutsidePath(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	baseHead := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD"))
	writeTestFile(t, filepath.Join(fixture.local, "image.png"), "image\n")
	runGit(t, fixture.local, "add", "image.png")
	writeTestFile(t, filepath.Join(fixture.local, "note.md"), "note\n")

	if err := w.cycle(context.Background(), time.Now()); err == nil {
		t.Fatal("cycle unexpectedly accepted an outside staged path")
	}
	if w.state != stateBlocked {
		t.Fatalf("state = %s, want blocked", w.state)
	}
	if got := strings.TrimSpace(runGit(t, fixture.local, "rev-parse", "HEAD")); got != baseHead {
		t.Fatal("worker created a commit while an outside path was staged")
	}
}

// TestInspectRejectsChangedBranchWiring verifies startup ownership checks.
func TestInspectRejectsChangedBranchWiring(t *testing.T) {
	t.Run("detached head", func(t *testing.T) {
		fixture := newTestRepository(t)
		runGit(t, fixture.local, "checkout", "--detach")
		client, err := gitx.NewClient()
		if err != nil {
			t.Skip(err)
		}
		if _, err := gitx.Inspect(context.Background(), client, fixture.local); err == nil {
			t.Fatal("Inspect() unexpectedly accepted detached HEAD")
		}
	})
	t.Run("missing upstream", func(t *testing.T) {
		fixture := newTestRepository(t)
		runGit(t, fixture.local, "branch", "--unset-upstream")
		client, err := gitx.NewClient()
		if err != nil {
			t.Skip(err)
		}
		if _, err := gitx.Inspect(context.Background(), client, fixture.local); err == nil {
			t.Fatal("Inspect() unexpectedly accepted a branch without upstream")
		}
	})
	t.Run("tracked conflict marker", func(t *testing.T) {
		fixture := newTestRepository(t)
		writeTestFile(t, filepath.Join(fixture.local, "CONFLICTS"), "reserved\n")
		runGit(t, fixture.local, "add", "CONFLICTS")
		runGit(t, fixture.local, "commit", "-m", "track reserved marker")
		client, err := gitx.NewClient()
		if err != nil {
			t.Skip(err)
		}
		if _, err := gitx.Inspect(context.Background(), client, fixture.local); err == nil {
			t.Fatal("Inspect() unexpectedly accepted tracked CONFLICTS")
		}
	})
}

// TestManagerLocksRepositoryAndStopsCleanly verifies timers, locking, and shutdown.
func TestManagerLocksRepositoryAndStopsCleanly(t *testing.T) {
	fixture := newTestRepository(t)
	client, err := gitx.NewClient()
	if err != nil {
		t.Skip(err)
	}
	repoConfig := config.Repository{
		Path:         fixture.local,
		Extensions:   []string{".md", ".txt"},
		SyncInterval: 100 * time.Millisecond,
	}
	cfg := config.Config{
		ScanInterval: 50 * time.Millisecond,
		Debounce:     50 * time.Millisecond,
		GitTimeout:   5 * time.Second,
		Repositories: []config.Repository{repoConfig},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := Prepare(context.Background(), cfg, client, &recordingNotifier{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), cfg, client, &recordingNotifier{}, logger); err == nil {
		manager.closeLocks()
		t.Fatal("second manager unexpectedly acquired the repository")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	writeTestFile(t, filepath.Join(fixture.local, "managed.md"), "managed by timers\n")

	deadline := time.Now().Add(8 * time.Second)
	for {
		output, err := tryGit(t.Context(), "", "--git-dir", fixture.remote, "show", fixture.branch+":managed.md")
		if err == nil && output == "managed by timers\n" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("manager did not push the timed change")
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Manager.Run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Run() did not stop after context cancellation")
	}

	restarted, err := Prepare(context.Background(), cfg, client, &recordingNotifier{}, logger)
	if err != nil {
		t.Fatalf("manager lock was not released after shutdown: %v", err)
	}
	restarted.closeLocks()
}

// cycleCommit advances a worker through detection and debounce.
func cycleCommit(t *testing.T, w *worker) {
	t.Helper()
	start := time.Unix(1_700_000_000, 0)
	if err := w.cycle(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := w.cycle(context.Background(), start.Add(w.debounce)); err != nil {
		t.Fatal(err)
	}
}

// newTestWorker creates a deterministic worker for integration tests.
func newTestWorker(t *testing.T, path string) (*worker, *recordingNotifier) {
	t.Helper()
	return newTestWorkerWithNotifier(t, path, &recordingNotifier{})
}

// newTestWorkerWithNotifier creates a worker with a supplied recorder.
func newTestWorkerWithNotifier(t *testing.T, path string, notifier *recordingNotifier) (*worker, *recordingNotifier) {
	t.Helper()
	client, err := gitx.NewClient()
	if err != nil {
		t.Skip(err)
	}
	repoConfig := config.Repository{
		Path:         path,
		Extensions:   []string{".md", ".txt"},
		SyncInterval: time.Minute,
	}
	cfg := config.Config{
		ScanInterval: time.Second,
		Debounce:     2 * time.Second,
		GitTimeout:   10 * time.Second,
		Repositories: []config.Repository{repoConfig},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := gitx.Inspect(ctx, client, path)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newWorker(cfg, repoConfig, info, client, notifier, logger), notifier
}

// newTestRepository creates an isolated remote and two clones.
func newTestRepository(t *testing.T) testRepository {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	fixture := testRepository{
		remote: filepath.Join(base, "remote.git"),
		local:  filepath.Join(base, "local"),
		peer:   filepath.Join(base, "peer"),
	}
	runGit(t, "", "init", "--bare", fixture.remote)
	runGit(t, "", "clone", fixture.remote, fixture.local)
	configureTestIdentity(t, fixture.local)
	writeTestFile(t, filepath.Join(fixture.local, "base.md"), "base\n")
	runGit(t, fixture.local, "add", "base.md")
	runGit(t, fixture.local, "commit", "-m", "initial")
	fixture.branch = strings.TrimSpace(runGit(t, fixture.local, "symbolic-ref", "--short", "HEAD"))
	runGit(t, fixture.local, "push", "-u", "origin", fixture.branch)
	runGit(t, "", "clone", fixture.remote, fixture.peer)
	configureTestIdentity(t, fixture.peer)
	return fixture
}

// configureTestIdentity installs a repository-local test identity.
func configureTestIdentity(t *testing.T, repo string) {
	t.Helper()
	runGit(t, repo, "config", "user.name", "notes-sync test")
	runGit(t, repo, "config", "user.email", "notes-sync@example.invalid")
}

// runGit executes a required Git fixture command.
func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	commandArgs := make([]string, 0, len(args)+2)
	if repo != "" {
		commandArgs = append(commandArgs, "-C", repo)
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(t.Context(), "git", commandArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", commandArgs, err, output)
	}
	return string(output)
}

// tryGit executes a Git command used for eventual assertions.
func tryGit(ctx context.Context, repo string, args ...string) (string, error) {
	commandArgs := make([]string, 0, len(args)+2)
	if repo != "" {
		commandArgs = append(commandArgs, "-C", repo)
	}
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	output, err := cmd.Output()
	return string(output), err
}

// writeTestFile writes one fixture file and its parent directory.
func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readTestFile reads one required fixture file.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

// recordingNotifier records conflict notification attempts.
type recordingNotifier struct {
	mu    sync.Mutex
	repos []string
}

// Conflict records one conflict notification.
func (n *recordingNotifier) Conflict(_ context.Context, repository string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.repos = append(n.repos, repository)
	return nil
}

// count returns the number of recorded notifications.
func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.repos)
}
