package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edementhon/kids-shelf/internal/apptest"
	"github.com/edementhon/kids-shelf/internal/launch"
)

// TestMain plays the fake app roles the end-to-end tests re-exec (the test
// binary as versioned builds under a fake install layout).
func TestMain(m *testing.M) {
	if apptest.RunIfRequested() {
		return
	}
	os.Exit(m.Run())
}

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(true, "", 0, true, "", "", &out, &errOut)
	if code != 0 {
		t.Fatalf("run(--version) = %d, want 0; stderr: %s", code, errOut.String())
	}
	if got := out.String(); got != "kids-shelf dev\n" {
		t.Fatalf("run(--version) printed %q, want %q", got, "kids-shelf dev\n")
	}
	if errOut.Len() != 0 {
		t.Fatalf("run(--version) wrote to stderr: %q", errOut.String())
	}
}

// TestUncreatableDataDirReportsError drives the real startup path up to the
// point where the data directory cannot be created: the error must reach
// stderr instead of exiting silently.
func TestUncreatableDataDirReportsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "sub") // a regular file is in the way

	var out, errOut bytes.Buffer
	code := run(false, dir, 0, true, "", "", &out, &errOut)
	if code == 0 {
		t.Fatal("run with an uncreatable data dir succeeded")
	}
	if !strings.Contains(errOut.String(), "kids-shelf:") {
		t.Fatalf("stderr has no kids-shelf error: %q", errOut.String())
	}
}

func TestFailWritesErrorFile(t *testing.T) {
	dir := t.TempDir()
	failErr := os.ErrNotExist
	var errOut bytes.Buffer
	code := fail(failErr, dir, true, &errOut)
	if code != 1 {
		t.Fatalf("fail() = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "kids-shelf:") {
		t.Fatalf("stderr has no kids-shelf error: %q", errOut.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "last-error.txt"))
	if err != nil {
		t.Fatalf("reading last-error.txt: %v", err)
	}
	if !strings.Contains(string(b), "kids-shelf could not start") {
		t.Fatalf("last-error.txt content: %q", b)
	}
}

// TestUncreatableDataDirShowsDialog replaces launch.ShowError with a
// recorder and drives the startup path whose data directory cannot be
// created or used: a native dialog must be shown exactly once (in addition
// to the stderr message), and --no-browser must suppress it.
func TestUncreatableDataDirShowsDialog(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "sub") // a regular file is in the way

	var calls []struct{ title, msg string }
	orig := launch.ShowError
	launch.ShowError = func(title, msg string) error {
		calls = append(calls, struct{ title, msg string }{title, msg})
		return nil
	}
	t.Cleanup(func() { launch.ShowError = orig })

	var out, errOut bytes.Buffer
	code := run(false, dir, 0, false, "", "", &out, &errOut)
	if code == 0 {
		t.Fatal("run with an uncreatable data dir succeeded")
	}
	if len(calls) != 1 {
		t.Fatalf("ShowError called %d times, want exactly once: %+v", len(calls), calls)
	}
	if calls[0].title != "kids-shelf" {
		t.Fatalf("dialog title = %q, want kids-shelf", calls[0].title)
	}
	if !strings.Contains(calls[0].msg, "kids-shelf could not start") {
		t.Fatalf("dialog message %q does not explain the failure", calls[0].msg)
	}
	if !strings.Contains(errOut.String(), "kids-shelf:") {
		t.Fatalf("stderr has no kids-shelf error: %q", errOut.String())
	}

	// --no-browser suppresses the dialog; the stderr message stays.
	calls = nil
	errOut.Reset()
	code = run(false, dir, 0, true, "", "", &out, &errOut)
	if code == 0 {
		t.Fatal("run with an uncreatable data dir succeeded with --no-browser")
	}
	if len(calls) != 0 {
		t.Fatalf("ShowError called %d times with --no-browser: %+v", len(calls), calls)
	}
	if !strings.Contains(errOut.String(), "kids-shelf:") {
		t.Fatalf("stderr has no kids-shelf error: %q", errOut.String())
	}
}
