package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestRefreshStateSeparatesTransitionsFromRepeatedFailures verifies that a
// repository stuck on one failure stops producing WARN entries while staying
// observable at debug level.
func TestRefreshStateSeparatesTransitionsFromRepeatedFailures(t *testing.T) {
	var buffer bytes.Buffer
	w := &worker{
		state:  stateHealthy,
		logger: slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	w.setDegraded("git fetch exited with status 128")
	w.setDegraded("git fetch exited with status 128")
	w.setDegraded("git fetch exited with status 1")
	w.clearDegraded()

	output := buffer.String()
	expected := []string{
		`level=WARN msg="repository state changed" state=degraded`,
		`level=DEBUG msg="repository state persists" state=degraded`,
		`level=WARN msg="repository state persists for a new reason" state=degraded`,
		`level=INFO msg="repository recovered" state=healthy`,
	}
	for _, want := range expected {
		if strings.Count(output, want) != 1 {
			t.Fatalf("want exactly one %q in:\n%s", want, output)
		}
	}
	if got := strings.Count(output, "level=WARN"); got != 2 {
		t.Fatalf("WARN entries = %d, want 2:\n%s", got, output)
	}
	if w.state != stateHealthy {
		t.Fatalf("state = %s, want healthy", w.state)
	}
}

// TestGitRunReportsStderrAtDebug verifies that the reason a Git command failed
// reaches the log, since the error itself carries only an exit status.
func TestGitRunReportsStderrAtDebug(t *testing.T) {
	fixture := newTestRepository(t)
	w, _ := newTestWorker(t, fixture.local)
	var buffer bytes.Buffer
	w.logger = slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, err := w.gitRun(context.Background(), "fetch", "missing-remote"); err == nil {
		t.Fatal("fetch from a missing remote unexpectedly succeeded")
	}

	output := buffer.String()
	if !strings.Contains(output, `msg="git command failed"`) || !strings.Contains(output, "operation=fetch") {
		t.Fatalf("git stderr was not reported at debug level:\n%s", output)
	}
	if !strings.Contains(output, "missing-remote") {
		t.Fatalf("debug entry does not explain the failure:\n%s", output)
	}
}
