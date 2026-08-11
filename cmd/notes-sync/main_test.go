package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecuteHelp verifies the documented command surface.
func TestExecuteHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("execute(help) code = %d", code)
	}
	if !strings.Contains(stdout.String(), "notes-sync run") || stderr.Len() != 0 {
		t.Fatalf("unexpected help output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestExecuteSubcommandHelp verifies flag help exits successfully.
func TestExecuteSubcommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"run", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("execute(run --help) code = %d", code)
	}
	if !strings.Contains(stderr.String(), "-config") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestExecuteRejectsUnknownCommand verifies usage failures.
func TestExecuteRejectsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("execute(unknown) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestExecuteReportsMissingConfig verifies startup configuration errors.
func TestExecuteReportsMissingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code := execute([]string{"check", "--config", missing}, &stdout, &stderr); code != 1 {
		t.Fatalf("execute(check) code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "open config") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
