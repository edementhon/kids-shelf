//go:build linux

package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pidAlive reports whether pid is a running (non-zombie) process. Orphans
// are reparented to PID 1, which may never reap them, so a zombie is
// treated as gone: SIGKILL(pid, 0) alone cannot distinguish a zombie from
// a live process.
func pidAlive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// Format: "pid (comm) state ..."; comm may contain spaces, so the
	// state is the first byte after the closing parenthesis.
	rest := b[strings.LastIndexByte(string(b), ')')+2:]
	return len(rest) > 0 && rest[0] != 'Z'
}

// TestSuperviseChildDiesWhenSupervisorKilled is the orphaned-child test:
// the supervisor holds a real child through the same lifeline wiring the
// launcher uses (lifeline pipe plus Pdeathsig on Linux). SIGKILLing the
// supervisor must take the child down with it - via PDEATHSIG or the pipe
// EOF watcher - within 6 s.
func TestSuperviseChildDiesWhenSupervisorKilled(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "child.pid")
	sup := exec.Command(os.Args[0])
	sup.Env = append(os.Environ(), "KS_TEST_ROLE=supervisor", "KS_SUP_PIDFILE="+pidfile)
	sup.Stdout = os.Stdout
	sup.Stderr = os.Stderr
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Process.Kill()

	// Wait for the supervisor to record the child's pid.
	var childPID int
	deadline := time.Now().Add(20 * time.Second)
	for childPID == 0 {
		b, err := os.ReadFile(pidfile)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the supervisor never recorded the child pid")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !pidAlive(childPID) {
		t.Fatalf("the child (pid %d) was not alive before the kill", childPID)
	}

	// kill -9 the supervisor; the child must be gone within 6 s.
	if err := sup.Process.Kill(); err != nil { // SIGKILL
		t.Fatal(err)
	}
	sup.Wait()
	goneBy := time.Now().Add(6 * time.Second)
	for pidAlive(childPID) {
		if time.Now().After(goneBy) {
			t.Fatalf("the child (pid %d) survived more than 6 s after the supervisor was killed", childPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
