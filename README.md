# notes-sync

A small per-user daemon for macOS and Linux that automatically commits selected
text files and synchronizes Git repositories with their upstream branches.

The first version deliberately uses polling instead of platform-specific file
system APIs. It has no external Go dependencies: building requires only the Go
standard library, while running requires an installed `git` executable. The
configuration format is JSON because the Go standard library does not include a
TOML parser.

## How it works

- Every `scan_interval`, the daemon reads machine-readable `git status` output.
- New, modified, deleted, and renamed files are filtered by extension. `.git`,
  `CONFLICTS`, and hidden path components are excluded by default, while
  `.gitignore` rules remain in effect.
- Once matching changes remain stable for the configured `debounce`, they are
  collected into one local commit such as
  `Notes: sync 2026-08-11T20:15:30+03:00`.
- After a commit, at startup, and every `sync_interval`, the daemon runs `fetch`,
  a regular merge of the tracking branch, and `push`.
- A network failure does not prevent local commits. Remote synchronization is
  retried on the next scheduled cycle.
- A merge conflict pauses only the affected repository and creates a
  `CONFLICTS` file in its root.

The extension filter limits automatic local commits. Merging the upstream
branch can still update any files already present in that branch.

## Repository requirements

Every configured path must be the root of a separate non-bare Git worktree and
must already have:

- an initial commit;
- a current branch rather than a detached HEAD;
- a configured tracking/upstream branch;
- `user.name` and `user.email` available through Git configuration;
- non-interactive authentication for the remote.

The daemon is designed to own a dedicated notes branch. Do not switch branches
or run concurrent index-changing operations such as `rebase` or `cherry-pick`.
The daemon never performs force-push, reset, checkout, or stash operations.

You can verify the repository setup with:

```sh
git -C /path/to/Notes status
git -C /path/to/Notes rev-parse '@{upstream}'
GIT_TERMINAL_PROMPT=0 git -C /path/to/Notes fetch
```

## Build and configuration

```sh
go build -o "$HOME/.local/bin/notes-sync" ./cmd/notes-sync
```

See [examples/config.json](examples/config.json) for a complete example:

```json
{
  "scan_interval": "2s",
  "debounce": "2s",
  "git_timeout": "60s",
  "log_level": "info",
  "notifications": true,
  "repositories": [
    {
      "path": "/absolute/path/to/Notes",
      "extensions": [".md", "*.txt"],
      "include_hidden": false,
      "sync_interval": "5m"
    }
  ]
}
```

Repository paths must be absolute and must not overlap. Simple forms such as
`.md` and `*.md` are equivalent; more complex glob expressions are not
supported. Extension matching is case-sensitive.

Default configuration paths:

- macOS: `~/Library/Application Support/notes-sync/config.json`;
- Linux: `$XDG_CONFIG_HOME/notes-sync/config.json`, or
  `~/.config/notes-sync/config.json` when `XDG_CONFIG_HOME` is not set.

Run the strict configuration and repository check before starting the service:

```sh
notes-sync check --config /absolute/path/config.json
notes-sync run --config /absolute/path/config.json
```

## Resolving conflicts

When a conflict occurs, the daemon leaves the Git merge open, creates
`CONFLICTS`, writes an error to the service log, and attempts to display a GUI
notification through `osascript` on macOS or an installed `notify-send` on
Linux.

```sh
cd /path/to/Notes
git status

# Edit the conflicting files, then stage only the resolved files:
git add -- resolved-note.md

# Removing the marker explicitly confirms the manual resolution:
rm CONFLICTS
```

On the next scan, the daemon verifies that no unmerged entries remain, runs
`git commit --no-edit`, and pushes the result. If `CONFLICTS` is removed too
early, the daemon recreates it. While the marker exists, new changes remain in
the worktree and are not committed. You may complete or abort the merge
manually, but the marker must still be removed before synchronization resumes.

## macOS autostart

The provided user LaunchAgent allows the process to access the user's files,
SSH agent, and GUI session.

1. Copy
   [examples/com.github.vladimirkvnk.notes-sync.plist](examples/com.github.vladimirkvnk.notes-sync.plist)
   to `~/Library/LaunchAgents/`.
2. Replace every `/Users/YOU` with the absolute path to your home directory.
3. Create `~/Library/Logs` if it does not already exist.
4. Validate and load the agent:

```sh
plutil -lint ~/Library/LaunchAgents/com.github.vladimirkvnk.notes-sync.plist
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.github.vladimirkvnk.notes-sync.plist
launchctl kickstart -k "gui/$(id -u)/com.github.vladimirkvnk.notes-sync"
tail -f ~/Library/Logs/notes-sync.log
```

Stop and unload it with:

```sh
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/com.github.vladimirkvnk.notes-sync.plist
```

Configure system-level rotation for the log file if needed.

## Linux autostart

```sh
mkdir -p ~/.config/systemd/user
cp examples/notes-sync.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now notes-sync.service
systemctl --user status notes-sync.service
journalctl --user -u notes-sync.service -f
```

If `notify-send` is unavailable or the user's D-Bus session cannot be reached,
the daemon continues to report conflicts through `CONFLICTS` and journald.

## Logging and states

Logs are written through `log/slog` to stderr. The default `info` level reports
startup, created commits, actual synchronization work, conflicts, and recovery.
Empty scan cycles are not logged, and repeated identical errors do not create
additional INFO or WARN entries.

Git errors are reported as a command and an exit status, because Git stderr can
contain credential URLs. Set `"log_level": "debug"` to see that stderr, along
with a `repository state persists` entry on every cycle a repository stays
degraded, blocked, or in conflict.

- `healthy` - normal operation;
- `degraded` - the remote is temporarily unavailable, while local commits
  continue;
- `blocked` - an unsafe Git state requires manual correction;
- `conflict` - a merge conflict is active together with `CONFLICTS`.

## Development and testing

Integration tests create temporary working repositories and a local bare
remote, so they do not require network access.

```sh
go test ./...
go test -race ./...
go vet ./...
make lint
go build ./cmd/notes-sync
make vuln
```

CI runs these checks on macOS and Linux and also validates the launchd plist and
systemd unit.
