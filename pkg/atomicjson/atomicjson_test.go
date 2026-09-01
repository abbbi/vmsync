/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

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

// What these tests do NOT cover, deliberately: durability across a power
// loss. No unit test can prove a rename survives one. TestWriteIsAtomic
// below is about a CONCURRENT READER -- it says nothing about what is on the
// platter afterwards. The fsync half of this package is defended by the
// reasoning at the bottom of Write and by review, and nothing here should be
// mistaken for evidence about it.
package atomicjson

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// doc is deliberately large. A document that fits in one page does not tear
// even when written with a plain os.WriteFile, so a small payload here would
// produce a test that passes against the very implementation it exists to
// rule out. 256 KiB spans enough pages that a non-atomic write is observable.
type doc struct {
	Seq int    `json:"seq"`
	Pad string `json:"pad"`
}

const padBytes = 256 * 1024

func newDoc(seq int) doc {
	return doc{Seq: seq, Pad: strings.Repeat("x", padBytes)}
}

// TestWriteIsAtomicUnderAConcurrentReader is the property this package is
// named for, and the one thing in it that can produce silent bad state.
//
// The failure it rules out: a reader catching a half-written file. For
// operations.json that means an operation which already RAN comes back
// parsed as absent -- or not parsed at all -- and Seen() lets it execute a
// second time. For a promote or a restore, twice is not a retry.
//
// The agent's own TestWriteIsAtomic (cmd/vmsync-agent/store_test.go) is
// named for this and does not test it: two SEQUENTIAL writes asserting
// last-writer-wins, which os.WriteFile also satisfies. This one races.
//
// Run under -race. Validated by breaking it: swapping Write's body for
// os.WriteFile must turn this red. If it stays green the payload is too
// small and the test is worthless.
func TestWriteIsAtomicUnderAConcurrentReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Not a shortcoming of the code -- a platform difference that makes
		// the EXPERIMENT impossible. MoveFileEx needs delete access to the
		// target, and a concurrent os.ReadFile holds a handle that denies it,
		// so it is the WRITER's rename that fails here ("Access is denied"),
		// not a reader that tears. On Linux a rename over an open file is
		// routine and the reader keeps the old inode, which is the whole
		// guarantee under test.
		//
		// TestWriteReplacesRatherThanModifies below covers the same
		// regression deterministically and does run here.
		t.Skip("a rename over a file held open by a reader is refused on windows, so the race cannot be staged")
	}

	const writes = 200

	path := filepath.Join(t.TempDir(), "operations.json")
	// Seed it, so the reader is never racing the very first create -- that
	// window is a missing file, not a torn one, and is not what is under test.
	if err := Write(path, newDoc(-1), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		torn     []string // the failure: a read that parsed as neither old nor new
		openErrs int      // platform noise: the file momentarily unopenable
		good     int
		lastSeq  = -2
	)

	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < writes; i++ {
			if err := Write(path, newDoc(i), 0o600); err != nil {
				mu.Lock()
				torn = append(torn, "write failed: "+err.Error())
				mu.Unlock()
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			b, err := os.ReadFile(path)
			if err != nil {
				// Not the failure under test. On Windows a rename over an
				// open file can momentarily refuse; on any platform this is
				// an all-or-nothing outcome, which is the guarantee, not a
				// violation of it.
				mu.Lock()
				openErrs++
				mu.Unlock()
				continue
			}
			var got doc
			if err := json.Unmarshal(b, &got); err != nil {
				mu.Lock()
				torn = append(torn, err.Error())
				mu.Unlock()
				continue
			}
			mu.Lock()
			good++
			// Content check, not just parseability: a document that parses
			// but whose padding was truncated is still a torn read, and JSON
			// would not necessarily notice.
			if len(got.Pad) != padBytes {
				torn = append(torn, "parsed but padding was truncated")
			}
			// The sequence must never go BACKWARDS. A reader that sees 7
			// after 12 has read a stale inode, which rename must make
			// impossible.
			if got.Seq < lastSeq {
				torn = append(torn, "sequence went backwards")
			}
			lastSeq = got.Seq
			mu.Unlock()
		}
	}()

	wg.Wait()

	if len(torn) > 0 {
		shown := torn
		if len(shown) > 5 {
			shown = shown[:5]
		}
		t.Errorf("%d torn reads out of %d successful ones; first few: %v", len(torn), good, shown)
	}
	// Without this the test is vacuous: a reader that never managed a single
	// read would report zero torn reads and pass.
	if good < 10 {
		t.Fatalf("only %d successful reads (and %d open failures) -- too few to have "+
			"raced the writer at all, so a green result here proves nothing", good, openErrs)
	}

	// And the writer's last word must be what is on disk.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	var final doc
	if err := json.Unmarshal(b, &final); err != nil {
		t.Fatalf("final read did not parse: %v", err)
	}
	if final.Seq != writes-1 {
		t.Errorf("final seq = %d, want %d", final.Seq, writes-1)
	}
}

// TestWriteReplacesRatherThanModifies is the deterministic half of the
// atomicity guarantee, and the one that runs on every platform.
//
// A rename installs a NEW inode over the old name; a write-in-place keeps
// the same one and passes through a truncated state on the way. So "did the
// file identity change" is a proxy for "was this atomic" that needs no race
// at all -- and it fails for exactly the regression the concurrent test
// exists to catch, namely Write being simplified into an os.WriteFile.
//
// Worth having even where the race test runs: this one cannot be flaky, and
// it names the mechanism rather than the symptom.
// Uses a HARD LINK rather than os.SameFile, and the difference matters.
// os.SameFile is useless for this: on Windows the file id is loaded lazily
// BY PATH, so two stats of one path both resolve to whatever is there now
// and it reports true across a rename that demonstrably changed the inode.
// A link pins the old inode by name, so this becomes a content assertion,
// which is portable and cannot be fooled.
func TestWriteReplacesRatherThanModifies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "operations.json")
	if err := Write(p, doc{Seq: 1}, 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "pinned.json")
	if err := os.Link(p, link); err != nil {
		// Not every filesystem has them. Skipping is honest; the concurrent
		// test covers the same ground where it can run.
		t.Skipf("cannot hard link on this filesystem: %v", err)
	}

	if err := Write(p, doc{Seq: 2}, 0o600); err != nil {
		t.Fatal(err)
	}

	// The link still names the ORIGINAL inode. A rename left it untouched;
	// a write-in-place would have rewritten it through the other name.
	b, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	var pinned doc
	if err := json.Unmarshal(b, &pinned); err != nil {
		t.Fatalf("parse the pinned inode: %v", err)
	}
	if pinned.Seq != 1 {
		t.Errorf("the pinned inode now reads seq=%d: the second write MODIFIED the existing file "+
			"instead of renaming a new one over it. A write-in-place passes through a truncated "+
			"state that any concurrent reader can observe, which for operations.json means an "+
			"operation that already ran can parse as absent and execute twice", pinned.Seq)
	}

	// And the real path must have moved on, or the test would pass for a
	// Write that did nothing at all.
	b, err = os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var current doc
	if err := json.Unmarshal(b, &current); err != nil {
		t.Fatal(err)
	}
	if current.Seq != 2 {
		t.Fatalf("the target reads seq=%d, want 2 -- the second write did not land", current.Seq)
	}
}

// TestPermIsExact pins tmp.Chmod, which is deletable today without a single
// test noticing: os.CreateTemp already creates at 0600, and the only mode
// assertion in the agent suite wants 0600. Chmod is what makes an 0644 file
// actually 0644, and every config.json, operations.json and fences.json goes
// through it.
//
// Exact equality rather than a mask, on purpose. A future switch to
// os.WriteFile would pass a masked check while letting umask quietly loosen
// the 0600 credentials file -- the agent's bearer token.
func TestPermIsExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Chmod on Windows only toggles the read-only bit, so every
		// writable file reports 0666 and the assertion cannot distinguish a
		// working Chmod from a deleted one. The agent ships to Linux.
		t.Skip("unix file modes are not modelled on windows")
	}

	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o644} {
		p := filepath.Join(dir, "m.json")
		if err := Write(p, doc{Seq: 1}, perm); err != nil {
			t.Fatalf("Write(%04o): %v", perm, err)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != perm {
			t.Errorf("mode = %04o, want %04o", got, perm)
		}
		os.Remove(p)
	}

	// Overwriting must install the TEMP file's mode, not leave the old
	// file's in place. This is what fails if somebody ever "simplifies"
	// Write into a write-in-place: the rename is what swaps the inode, and
	// with it the mode.
	p := filepath.Join(dir, "tightened.json")
	if err := Write(p, doc{Seq: 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, doc{Seq: 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o644 {
		t.Errorf("after rewriting an 0600 file with perm 0644 the mode is %04o, want 0644 -- "+
			"the rename must install the temp file's inode and mode, not update in place", got)
	}
}

// TestADirectorySyncFailureDoesNotFailTheWrite guards a specific and likely
// regression, not a hypothetical one.
//
// A linter flags `_ = syncDir(dir)` as an unchecked error. Returning it looks
// obviously correct. It is not: per Write's own reasoning the data is already
// written, flushed and renamed by that point, so the only thing an error
// there reports is that the directory entry's durability could not be
// CONFIRMED. Propagating it tells callers the write did not happen, and they
// act on that -- operationLedger.Begin refuses to execute. That trades an
// availability outage for a durability guarantee that was never obtainable.
//
// The agent's version of this test (store_test.go) stages no failure at all:
// on Linux, fsync on a directory succeeds, so it degrades to "SaveCache
// works". This one injects the refusal.
func TestADirectorySyncFailureDoesNotFailTheWrite(t *testing.T) {
	sentinel := errors.New("simulated directory fsync refusal")
	orig := syncDir
	syncDir = func(string) error { return sentinel }
	t.Cleanup(func() { syncDir = orig })

	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := Write(p, doc{Seq: 7}, 0o600); err != nil {
		t.Fatalf("Write returned %v; a directory fsync that refuses must not fail the write, "+
			"because by that point the data is already written, flushed and renamed", err)
	}

	// And it must be a real write, not an early return that happened to be
	// nil: the contents have to be there.
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got doc
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Seq != 7 {
		t.Errorf("seq = %d, want 7", got.Seq)
	}
}

// TestSyncDirReportsAFailureToItsCaller is the other half: SyncDir itself
// returns its error, even though Write chooses to ignore it. The choice
// belongs to the caller, and a future caller that CAN act on it needs the
// error to exist.
func TestSyncDirReportsAFailureToItsCaller(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "not-a-directory")); err == nil {
		t.Error("SyncDir on a missing directory returned nil")
	}
}

// TestWriteCreatesAMissingParent covers the MkdirAll at the top, which is
// load-bearing for a first run on a host where the state dir does not exist
// yet.
func TestWriteCreatesAMissingParent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "deeper", "cfg.json")
	if err := Write(p, doc{Seq: 1}, 0o600); err != nil {
		t.Fatalf("Write into a missing parent: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat after write: %v", err)
	}
}

// TestWriteLeavesNoTempFileBehind on both paths. The success path is what
// keeps a state directory from filling up over months of polling; the
// failure path is the only one where the deferred remove does any work, and
// nothing exercised it before.
func TestWriteLeavesNoTempFileBehind(t *testing.T) {
	t.Run("on success", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "cfg.json")
		for i := 0; i < 5; i++ {
			if err := Write(p, doc{Seq: i}, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		assertOnlyJSON(t, dir)
	})

	t.Run("when the rename cannot happen", func(t *testing.T) {
		// A DIRECTORY where the file should go. MkdirAll and CreateTemp both
		// succeed -- the temp file is genuinely created -- and os.Rename is
		// what refuses. That is the only cheap way to reach a failure after
		// the temp file exists, and so the only way to exercise the deferred
		// remove doing real work.
		dir := t.TempDir()
		p := filepath.Join(dir, "cfg.json")
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := Write(p, doc{Seq: 1}, 0o600); err == nil {
			t.Fatal("Write reported success when its target path is a directory")
		}
		// The temp file must be gone even though the write failed.
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.Contains(e.Name(), ".tmp") {
				t.Errorf("a failed write left %q behind; over a failure loop this fills the state dir", e.Name())
			}
		}
	})
}

func assertOnlyJSON(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" {
			t.Errorf("state dir holds leftover %q, want only .json files", e.Name())
		}
	}
}
