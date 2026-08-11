package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

// ErrUnavailable reports an unsupported notification environment.
var ErrUnavailable = errors.New("desktop notifications are unavailable")

// OS sends notifications through optional platform commands.
type OS struct {
	enabled bool
}

// New creates an operating-system notifier.
func New(enabled bool) *OS {
	return &OS{enabled: enabled}
}

// Conflict reports a paused repository to the desktop session.
func (n *OS) Conflict(ctx context.Context, repository string) error {
	if !n.enabled {
		return nil
	}
	title := "notes-sync conflict"
	body := fmt.Sprintf("Synchronization paused for %s. Open CONFLICTS for instructions.", repository)

	switch runtime.GOOS {
	case "darwin":
		cmd := exec.CommandContext(ctx, "/usr/bin/osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run",
			title, body,
		)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("send macOS notification: %w", err)
		}
		return nil
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return ErrUnavailable
		}
		if err := exec.CommandContext(ctx, path, "--app-name=notes-sync", "--", title, body).Run(); err != nil {
			return fmt.Errorf("send Linux notification: %w", err)
		}
		return nil
	default:
		return ErrUnavailable
	}
}
