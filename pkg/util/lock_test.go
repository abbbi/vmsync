/*
	Copyright (C) 2026  Michael Ablassmeier <abi@grinser.de>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package util

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain intercepts runs of this test binary that were re-exec'd as the
// SIGKILL-test helper subprocess (see TestAcquireRunLock_SurvivesSIGKILL),
// so that only the helper logic runs in the child -- rather than the full
// test suite -- following the same idiom used by package os/exec's own
// tests.
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		runHelperProcess()
		// runHelperProcess never returns on success (it blocks forever in
		// select{} while holding the lock, until the parent test kills this
		// process); it only returns control here by calling os.Exit(1) on
		// failure. Nothing reachable follows.
		return
	}
	os.Exit(m.Run())
}

// runHelperProcess is the body executed by the re-exec'd child process. It
// acquires the lock named by LOCK_DIR/LOCK_KEY, signals readiness on
// stdout, and then blocks forever so the parent test can SIGKILL it while
// it still holds the flock.
func runHelperProcess() {
	dir := os.Getenv("LOCK_DIR")
	key := os.Getenv("LOCK_KEY")

	f, err := AcquireRunLock(dir, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: AcquireRunLock(%q, %q) failed: %v\n", dir, key, err)
		os.Exit(1)
	}
	_ = f // keep the *os.File (and its fd, and the flock) alive

	fmt.Println("LOCKED")

	select {} // block forever; the parent test terminates us via SIGKILL
}

// TestHelperProcess is never actually run as a test: TestMain intercepts
// the GO_WANT_HELPER_PROCESS re-exec before testing.M.Run is ever called,
// so this body is unreachable. It exists only so that the child process's
// "-test.run=^TestHelperProcess$" argument names a real test function, per
// the standard os/exec-style self-re-exec idiom.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
}

func TestAcquireRunLock_Basic(t *testing.T) {
	dir := t.TempDir()
	key := "some-domain"

	f, err := AcquireRunLock(dir, key)
	if err != nil {
		t.Fatalf("AcquireRunLock() error = %v, want nil", err)
	}
	defer f.Close()

	wantPath := filepath.Join(dir, safeKeyReplacer.Replace(key)+".lock")
	if _, statErr := os.Stat(wantPath); statErr != nil {
		t.Fatalf("expected lock file %s to exist, stat error: %v", wantPath, statErr)
	}
}

func TestAcquireRunLock_SecondAcquireFailsWhileHeld(t *testing.T) {
	dir := t.TempDir()
	key := "some-domain"

	f, err := AcquireRunLock(dir, key)
	if err != nil {
		t.Fatalf("first AcquireRunLock() error = %v, want nil", err)
	}
	defer f.Close()

	f2, err2 := AcquireRunLock(dir, key)
	if err2 == nil {
		t.Fatalf("second AcquireRunLock() error = nil, want non-nil (lock already held)")
	}
	if f2 != nil {
		f2.Close()
		t.Fatalf("second AcquireRunLock() returned non-nil *os.File = %v, want nil", f2)
	}
}

func TestAcquireRunLock_SucceedsAfterRelease(t *testing.T) {
	dir := t.TempDir()
	key := "some-domain"

	f, err := AcquireRunLock(dir, key)
	if err != nil {
		t.Fatalf("first AcquireRunLock() error = %v, want nil", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	f2, err2 := AcquireRunLock(dir, key)
	if err2 != nil {
		t.Fatalf("AcquireRunLock() after release error = %v, want nil", err2)
	}
	defer f2.Close()
}

func TestAcquireRunLock_DistinctKeysDoNotCollide(t *testing.T) {
	dir := t.TempDir()

	// "a/b" and "a b" would sanitize to the same "a_b" under a naive lossy
	// substitution scheme; safeKeyReplacer's percent-encoding style must
	// keep them distinct so both locks can be held concurrently without
	// contention between unrelated keys.
	keyA := "a/b"
	keyB := "a b"

	fA, errA := AcquireRunLock(dir, keyA)
	if errA != nil {
		t.Fatalf("AcquireRunLock(%q) error = %v, want nil", keyA, errA)
	}
	defer fA.Close()

	fB, errB := AcquireRunLock(dir, keyB)
	if errB != nil {
		t.Fatalf("AcquireRunLock(%q) error = %v, want nil", keyB, errB)
	}
	defer fB.Close()

	pathA := filepath.Join(dir, safeKeyReplacer.Replace(keyA)+".lock")
	pathB := filepath.Join(dir, safeKeyReplacer.Replace(keyB)+".lock")
	if pathA == pathB {
		t.Fatalf("expected distinct lock file paths for %q and %q, both resolved to %s", keyA, keyB, pathA)
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Errorf("expected lock file %s to exist, stat error: %v", pathA, err)
	}
	if _, err := os.Stat(pathB); err != nil {
		t.Errorf("expected lock file %s to exist, stat error: %v", pathB, err)
	}
}

func TestSafeKeyReplacer(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"slash", "a/b", "a%2fb"},
		{"space", "a b", "a%20b"},
		{"literal percent", "50%", "50%%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeKeyReplacer.Replace(tt.key); got != tt.want {
				t.Errorf("safeKeyReplacer.Replace(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}

	// The three keys above must sanitize to pairwise-distinct strings --
	// that is the entire point of percent-encoding instead of lossy
	// substitution (e.g. collapsing both "/" and " " to "_").
	sanitized := map[string]string{
		"a/b": safeKeyReplacer.Replace("a/b"),
		"a b": safeKeyReplacer.Replace("a b"),
		"50%": safeKeyReplacer.Replace("50%"),
	}
	seen := make(map[string]string, len(sanitized))
	for original, s := range sanitized {
		if prevOriginal, ok := seen[s]; ok {
			t.Errorf("keys %q and %q both sanitize to %q, expected distinct results", prevOriginal, original, s)
		}
		seen[s] = original
	}
}

// TestAcquireRunLock_SurvivesSIGKILL verifies the doc comment's claim that
// the kernel releases a process's flock automatically when its file
// descriptor closes for any reason -- including SIGKILL -- by acquiring the
// lock in a re-exec'd child process, killing that child with SIGKILL while
// it still holds the lock, and then confirming the parent process can
// immediately acquire the same lock.
func TestAcquireRunLock_SurvivesSIGKILL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock/SIGKILL semantics are POSIX-only; lock.go itself is Unix-only")
	}

	dir := t.TempDir()
	key := "sigkill-test-domain"

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"LOCK_DIR="+dir,
		"LOCK_KEY="+key,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// Ensure we never leak the child even if an assertion below fails
	// before we get a chance to SIGKILL it deliberately.
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	readyErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		line, err := reader.ReadString('\n')
		if err != nil {
			readyErr <- fmt.Errorf("reading helper stdout: %w", err)
			return
		}
		if strings.TrimSpace(line) != "LOCKED" {
			readyErr <- fmt.Errorf("unexpected helper readiness line: %q", line)
			return
		}
		readyErr <- nil
	}()

	select {
	case err := <-readyErr:
		if err != nil {
			t.Fatalf("helper process failed to signal readiness: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for helper process to acquire the lock (stderr: %s)", stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("Signal(SIGKILL) error = %v", err)
	}

	// The process was killed, so Wait is expected to report a non-nil
	// error (e.g. "signal: killed"); that is expected and not a test
	// failure in itself.
	_ = cmd.Wait()

	f, err := AcquireRunLock(dir, key)
	if err != nil {
		t.Fatalf("AcquireRunLock() after SIGKILL of lock holder: error = %v, want nil", err)
	}
	defer f.Close()
}
