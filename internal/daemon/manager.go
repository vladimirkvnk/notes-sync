package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/vladimirkvnk/notes-sync/internal/config"
	"github.com/vladimirkvnk/notes-sync/internal/gitx"
	"github.com/vladimirkvnk/notes-sync/internal/lockfile"
)

// Notifier reports a repository conflict to the desktop session.
type Notifier interface {
	Conflict(ctx context.Context, repository string) error
}

// Manager owns all repository locks and workers for one daemon process.
type Manager struct {
	workers []*worker
	locks   []*lockfile.Lock
	logger  *slog.Logger
}

// InspectRepositories performs all non-network startup validation.
func InspectRepositories(ctx context.Context, cfg config.Config, git *gitx.Client) ([]gitx.RepositoryInfo, error) {
	infos := make([]gitx.RepositoryInfo, 0, len(cfg.Repositories))
	for _, repo := range cfg.Repositories {
		commandCtx, cancel := context.WithTimeout(ctx, cfg.GitTimeout)
		info, err := gitx.Inspect(commandCtx, git, repo.Path)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("repository %q: %w", repo.Path, err)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// ValidateWorkingState checks whether a repository can be safely managed now.
// Matching staged files are allowed because the daemon owns those paths and
// can recover them after an interrupted commit.
func ValidateWorkingState(
	ctx context.Context,
	repo config.Repository,
	info gitx.RepositoryInfo,
	git *gitx.Client,
	timeout time.Duration,
) error {
	marker, err := pathExists(filepath.Join(info.Root, "CONFLICTS"))
	if err != nil {
		return err
	}
	if marker {
		return errors.New("CONFLICTS marker exists")
	}
	operation, err := inProgressOperation(info.GitDir)
	if err != nil {
		return err
	}
	if operation != "" {
		return fmt.Errorf("git %s operation is in progress", operation)
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := git.Run(commandCtx, info.Root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return fmt.Errorf("read Git status: %w", err)
	}
	entries, err := ParseStatus(raw)
	if err != nil {
		return err
	}
	_, unmerged, stagedOutside := splitStatus(repo, entries)
	if len(unmerged) > 0 {
		return errors.New("repository contains unresolved conflicts")
	}
	if len(stagedOutside) > 0 {
		return fmt.Errorf("staged paths outside configured extensions: %s", summarizePaths(stagedOutside))
	}
	return nil
}

// Prepare validates repositories and atomically acquires every repository
// lock. If one lock cannot be acquired, already acquired locks are released.
func Prepare(
	ctx context.Context,
	cfg config.Config,
	git *gitx.Client,
	notifier Notifier,
	logger *slog.Logger,
) (*Manager, error) {
	infos, err := InspectRepositories(ctx, cfg, git)
	if err != nil {
		return nil, err
	}

	manager := &Manager{logger: logger}
	for i, info := range infos {
		lock, err := lockfile.Acquire(filepath.Join(info.GitDir, "notes-sync.lock"))
		if err != nil {
			manager.closeLocks()
			return nil, fmt.Errorf("repository %q: %w", info.Root, err)
		}
		manager.locks = append(manager.locks, lock)
		manager.workers = append(manager.workers, newWorker(cfg, cfg.Repositories[i], info, git, notifier, logger))
	}
	return manager, nil
}

// Run runs all repository workers until ctx is canceled.
func (m *Manager) Run(ctx context.Context) error {
	defer m.closeLocks()
	m.logger.Info("notes-sync started", "repositories", len(m.workers))

	var wg sync.WaitGroup
	errorsCh := make(chan error, len(m.workers))
	for _, repoWorker := range m.workers {
		wg.Go(func() {
			if err := repoWorker.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errorsCh <- err
			}
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		<-done
		m.logger.Info("notes-sync stopped")
		return nil
	case err := <-errorsCh:
		return err
	case <-done:
		return nil
	}
}

// closeLocks releases every acquired repository lock.
func (m *Manager) closeLocks() {
	for _, lock := range slices.Backward(m.locks) {
		_ = lock.Close()
	}
	m.locks = nil
}
