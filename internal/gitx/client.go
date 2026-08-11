package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Client executes Git without involving a shell.
type Client struct {
	path string
}

// CommandError is a deliberately terse Git error. Stderr remains available to
// callers but is not included in Error, because it can contain credential URLs.
type CommandError struct {
	Operation string
	Code      int
	Stderr    string
	Cause     error
}

// Error returns a credential-safe command summary.
func (e *CommandError) Error() string {
	if errors.Is(e.Cause, context.DeadlineExceeded) {
		return fmt.Sprintf("git %s timed out", e.Operation)
	}
	if errors.Is(e.Cause, context.Canceled) {
		return fmt.Sprintf("git %s canceled", e.Operation)
	}
	if e.Code >= 0 {
		return fmt.Sprintf("git %s exited with status %d", e.Operation, e.Code)
	}
	return fmt.Sprintf("git %s failed", e.Operation)
}

// Unwrap exposes the underlying process or context error.
func (e *CommandError) Unwrap() error { return e.Cause }

// NewClient locates the Git executable.
func NewClient() (*Client, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("git executable was not found in PATH")
	}
	return &Client{path: path}, nil
}

// Run runs Git in repo and returns stdout. Git stderr is returned only through
// CommandError and should not be emitted at normal log levels.
func (c *Client) Run(ctx context.Context, repo string, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if repo != "" {
		cmdArgs = append(cmdArgs, "-C", repo)
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.CommandContext(ctx, c.path, cmdArgs...)
	cmd.Env = environmentWith(os.Environ(), map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"LC_ALL":              "C",
	})
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}

	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	cause := err
	if ctx.Err() != nil {
		cause = ctx.Err()
	}
	operation := "command"
	if len(args) > 0 {
		operation = args[0]
	}
	return stdout.String(), &CommandError{
		Operation: operation,
		Code:      code,
		Stderr:    stderr.String(),
		Cause:     cause,
	}
}

// environmentWith replaces environment values without duplicates.
func environmentWith(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[key]; overridden {
				continue
			}
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

// RepositoryInfo contains the immutable repository wiring checked at startup.
type RepositoryInfo struct {
	Root     string
	GitDir   string
	Name     string
	Branch   string
	Remote   string
	MergeRef string
	Upstream string
}

// Inspect validates a configured repository without accessing the network.
func Inspect(ctx context.Context, client *Client, configuredPath string) (RepositoryInfo, error) {
	root, err := runTrimmed(ctx, client, configuredPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("not a Git worktree: %w", err)
	}
	root, err = filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("resolve Git root: %w", err)
	}
	configuredPath, err = filepath.EvalSymlinks(filepath.Clean(configuredPath))
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("resolve configured path: %w", err)
	}
	if root != configuredPath {
		return RepositoryInfo{}, fmt.Errorf("configured path is not the worktree root (Git root is %q)", root)
	}

	bare, err := runTrimmed(ctx, client, root, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "false" {
		return RepositoryInfo{}, errors.New("bare repositories are not supported")
	}
	if _, err := client.Run(ctx, root, "rev-parse", "--verify", "HEAD"); err != nil {
		return RepositoryInfo{}, errors.New("repository must contain an initial commit")
	}
	branch, err := runTrimmed(ctx, client, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return RepositoryInfo{}, errors.New("repository is in detached HEAD state")
	}
	remote, err := runTrimmed(ctx, client, root, "config", "--get", "branch."+branch+".remote")
	if err != nil || remote == "" {
		return RepositoryInfo{}, fmt.Errorf("branch %q has no configured upstream remote", branch)
	}
	mergeRef, err := runTrimmed(ctx, client, root, "config", "--get", "branch."+branch+".merge")
	if err != nil || !strings.HasPrefix(mergeRef, "refs/heads/") {
		return RepositoryInfo{}, fmt.Errorf("branch %q has no supported upstream branch", branch)
	}
	upstream, err := runTrimmed(ctx, client, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("resolve upstream for branch %q: %w", branch, err)
	}
	if _, err := client.Run(ctx, root, "rev-parse", "--verify", "@{upstream}"); err != nil {
		return RepositoryInfo{}, fmt.Errorf("upstream %q does not exist locally; fetch it before starting", upstream)
	}
	if _, err := client.Run(ctx, root, "var", "GIT_AUTHOR_IDENT"); err != nil {
		return RepositoryInfo{}, errors.New("git author identity is not configured")
	}
	trackedConflict, err := client.Run(ctx, root, "ls-files", "--", "CONFLICTS")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("check CONFLICTS path: %w", err)
	}
	if strings.TrimSpace(trackedConflict) != "" {
		return RepositoryInfo{}, errors.New("CONFLICTS is tracked by Git and must be removed from the index")
	}

	gitDir, err := runTrimmed(ctx, client, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("resolve Git directory: %w", err)
	}
	gitDir, err = filepath.Abs(gitDir)
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("resolve absolute Git directory: %w", err)
	}

	return RepositoryInfo{
		Root:     root,
		GitDir:   gitDir,
		Name:     filepath.Base(root),
		Branch:   branch,
		Remote:   remote,
		MergeRef: mergeRef,
		Upstream: upstream,
	}, nil
}

// RuntimeWiringMatches verifies that nobody switched the managed branch or
// rewired its upstream while the daemon was running.
func RuntimeWiringMatches(ctx context.Context, client *Client, info RepositoryInfo) error {
	branch, err := runTrimmed(ctx, client, info.Root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch != info.Branch {
		return fmt.Errorf("current branch changed; expected %q", info.Branch)
	}
	upstream, err := runTrimmed(ctx, client, info.Root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil || upstream != info.Upstream {
		return fmt.Errorf("upstream changed; expected %q", info.Upstream)
	}
	return nil
}

// runTrimmed runs Git and trims stdout.
func runTrimmed(ctx context.Context, client *Client, repo string, args ...string) (string, error) {
	value, err := client.Run(ctx, repo, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// ShortOID returns a stable short object name for logs.
func ShortOID(oid string) string {
	oid = strings.TrimSpace(oid)
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}
