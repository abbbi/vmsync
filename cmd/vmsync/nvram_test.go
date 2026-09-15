package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

const nv = "/var/lib/libvirt/qemu/nvram/web01_VARS.fd"

// Minimal domain XML, real enough for libvirtxml to parse and for DetectNvram
// to find (or not find) an <os><nvram>. The <loader> is present because a UEFI
// domain always has one and because nothing here must ever touch it -- that
// file belongs to the target host, not to the VM.
const xmlWithNvram = `<domain type="kvm"><name>web01</name><os>` +
	`<type arch="x86_64" machine="q35">hvm</type>` +
	`<loader readonly="yes" type="pflash">/usr/share/OVMF/OVMF_CODE.fd</loader>` +
	`<nvram>` + nv + `</nvram></os></domain>`

const xmlNoNvram = `<domain type="kvm"><name>web01</name><os>` +
	`<type arch="x86_64" machine="q35">hvm</type></os></domain>`

func unq(s string) string { return strings.Trim(s, "'") }

// fakeHost models one host's filesystem and the shell commands this code runs
// against it.
//
// It deliberately prepends a login banner to every command's output, because
// that is what the real runners do: both merge stderr into stdout, and these
// hosts print an MOTD. A payload that cannot survive it cannot survive
// production.
type fakeHost struct {
	files        map[string][]byte
	cmds         []string
	mutateOnRead int // while > 0, the file changes after each read
	generation   int
	quiet        bool // suppress the banner, to prove it is the marker doing the work
}

func (h *fakeHost) noise() string {
	if h.quiet {
		return ""
	}
	return "NetPerfect - Private System\nUNAUTHORIZED ACCESS TO THIS DEVICE IS PROHIBITED\n"
}

// argAfter returns the first shell word following marker in the command.
func argAfter(cmd, marker string) string {
	rest := strings.SplitN(cmd, marker, 2)[1]
	// Trailing ";" as well: the real command is
	//   printf ...; base64 -w0 PATH; echo
	// so the first field after the marker carries the separator.
	return strings.Trim(strings.Fields(strings.TrimSpace(rest))[0], "';")
}

func (h *fakeHost) Run(ctx context.Context, cmd string) (string, error) {
	h.cmds = append(h.cmds, cmd)
	switch {
	case strings.Contains(cmd, "sha256sum "):
		p := argAfter(cmd, "sha256sum ")
		b, ok := h.files[p]
		if !ok {
			return h.noise() + nvramMarker + "\n", nil // present, empty payload
		}
		return h.noise() + nvramMarker + sum(b) + "\n", nil

	case strings.Contains(cmd, "base64 -w0 "):
		p := argAfter(cmd, "base64 -w0 ")
		b, ok := h.files[p]
		if !ok {
			return "", fmt.Errorf("no such file")
		}
		out := base64.StdEncoding.EncodeToString(b)
		if h.mutateOnRead > 0 {
			h.mutateOnRead--
			h.generation++
			h.files[p] = []byte(fmt.Sprintf("varstore-generation-%d", h.generation))
		}
		return h.noise() + nvramMarker + out + "\n", nil

	case strings.HasPrefix(cmd, "rm -f "):
		delete(h.files, argAfter(cmd, "rm -f "))
		return "", nil

	case strings.HasPrefix(cmd, "chown "), strings.HasPrefix(cmd, "chmod "):
		return "", nil

	case strings.HasPrefix(cmd, "mv -f "):
		f := strings.Fields(strings.TrimPrefix(cmd, "mv -f "))
		src, dst := unq(f[0]), unq(f[1])
		h.files[dst] = h.files[src]
		delete(h.files, src)
		return "", nil
	}
	return "", fmt.Errorf("unexpected command %q", cmd)
}

func (h *fakeHost) writer(owner string) *remoteFileWriter {
	return &remoteFileWriter{
		runner: h,
		owner:  owner,
		input: func(ctx context.Context, cmd string, data []byte) error {
			h.cmds = append(h.cmds, cmd)
			p := unq(strings.TrimSpace(strings.TrimPrefix(cmd, "cat > ")))
			h.files[p] = append([]byte(nil), data...)
			return nil
		},
	}
}

func (h *fakeHost) ran(substr string) int {
	n := 0
	for _, c := range h.cmds {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// Both runners merge stderr into stdout, so a host with an MOTD interleaves it
// with every payload. Binary transfer is unforgiving about that.
func TestPayloadSurvivesALoginBanner(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{nv: []byte("varstore-bytes")}}
	data, got, err := readFileVerified(context.Background(), h, nv)
	if err != nil {
		t.Fatalf("a banner on stdout broke the transfer: %v", err)
	}
	if string(data) != "varstore-bytes" || got != sum(data) {
		t.Errorf("the banner corrupted the payload: got %q", data)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{}}
	s, ok, err := fileSHA256(context.Background(), h, nv)
	if err != nil {
		t.Fatalf("a missing file was reported as an error: %v", err)
	}
	if ok || s != "" {
		t.Errorf("a missing file was reported as present (ok=%v sum=%q)", ok, s)
	}
}

// A command that did not run at all must not look like a file that is absent.
func TestUnmarkedOutputIsAnErrorNotAnAbsentFile(t *testing.T) {
	if _, err := markedOutput("sha256sum: command not found\n", "hashing x"); err == nil {
		t.Fatal("output with no marker was accepted; a broken host would look like an empty varstore")
	}
}

// The torn-read question: a write landing mid-read must never be committed.
func TestReadRetriesWhenTheFileChangesUnderneath(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{nv: []byte("original")}, mutateOnRead: 1}
	data, got, err := readFileVerified(context.Background(), h, nv)
	if err != nil {
		t.Fatalf("a single mid-read change was not recovered from: %v", err)
	}
	if got != sum(data) {
		t.Error("the returned hash does not describe the returned bytes")
	}
	if got != sum(h.files[nv]) {
		t.Error("returned a copy that does not match the file on the host -- a torn read was committed")
	}
	if string(data) == "original" {
		t.Error("returned the pre-change bytes; the retry did not actually re-read")
	}
}

func TestReadGivesUpWhenTheFileNeverSettles(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{nv: []byte("original")}, mutateOnRead: 99}
	if _, _, err := readFileVerified(context.Background(), h, nv); err == nil {
		t.Fatal("a file that changes on every read was accepted; a torn copy could be committed")
	}
}

func TestStableFileIsReadInOneAttempt(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{nv: []byte("stable-varstore")}, quiet: true}
	data, got, err := readFileVerified(context.Background(), h, nv)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "stable-varstore" || got != sum(data) {
		t.Error("a stable read returned the wrong content")
	}
	if n := h.ran("base64"); n != 1 {
		t.Errorf("a stable file was read %d times, want 1", n)
	}
}

// Ownership and mode must be set BEFORE the rename: qemu runs unprivileged and
// a varstore it cannot open stops the domain from starting at all.
func TestWriteChownsBeforeRenaming(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{}}
	data := []byte("new-varstore")
	if err := h.writer("qemu:qemu").write(context.Background(), nv, data, sum(data)); err != nil {
		t.Fatal(err)
	}
	if string(h.files[nv]) != "new-varstore" {
		t.Fatal("the file was not installed at the destination")
	}
	if _, stillTmp := h.files[nv+".vmsync-tmp"]; stillTmp {
		t.Error("the temp file was left behind")
	}
	idx := func(prefix string) int {
		for i, c := range h.cmds {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	chown, chmod, mv := idx("chown "), idx("chmod "), idx("mv -f ")
	if chown < 0 || chmod < 0 || mv < 0 {
		t.Fatalf("a step is missing: chown=%d chmod=%d mv=%d", chown, chmod, mv)
	}
	if chown > mv || chmod > mv {
		t.Errorf("ownership/mode applied after the rename (chown=%d chmod=%d mv=%d)", chown, chmod, mv)
	}
}

// A transfer that arrives corrupt must never be renamed into place.
func TestCorruptTransferIsDiscardedNotInstalled(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{nv: []byte("existing-good-varstore")}}
	err := h.writer("qemu:qemu").write(context.Background(), nv,
		[]byte("arrived-corrupt"), sum([]byte("something-else")))
	if err == nil {
		t.Fatal("a corrupt transfer was accepted")
	}
	if string(h.files[nv]) != "existing-good-varstore" {
		t.Error("the existing varstore was overwritten by a corrupt transfer")
	}
	if _, stillTmp := h.files[nv+".vmsync-tmp"]; stillTmp {
		t.Error("the corrupt temp file was left behind")
	}
}

func TestNonUEFIGuestIsANoOp(t *testing.T) {
	src := &fakeHost{files: map[string][]byte{}}
	tgt := &fakeHost{files: map[string][]byte{}}
	if err := syncNvram(context.Background(), src, tgt.writer(""), xmlNoNvram, "web01", "web01"); err != nil {
		t.Fatal(err)
	}
	if n := len(src.cmds) + len(tgt.cmds); n != 0 {
		t.Errorf("a non-UEFI guest ran %d commands; it should run none", n)
	}
}

// The common case by far: nothing changed, so nothing is copied.
func TestUnchangedVarstoreIsNotCopied(t *testing.T) {
	same := []byte("identical-varstore")
	src := &fakeHost{files: map[string][]byte{nv: same}}
	tgt := &fakeHost{files: map[string][]byte{nv: same}}
	if err := syncNvram(context.Background(), src, tgt.writer("qemu:qemu"), xmlWithNvram, "web01", "web01"); err != nil {
		t.Fatal(err)
	}
	if n := src.ran("base64") + tgt.ran("cat >") + tgt.ran("mv -f "); n != 0 {
		t.Errorf("an unchanged varstore was copied anyway (%d transfer commands)", n)
	}
}

func TestChangedVarstoreIsCopied(t *testing.T) {
	src := &fakeHost{files: map[string][]byte{nv: []byte("source-has-new-boot-entry")}}
	tgt := &fakeHost{files: map[string][]byte{nv: []byte("target-is-stale")}}
	if err := syncNvram(context.Background(), src, tgt.writer("qemu:qemu"), xmlWithNvram, "web01", "web01"); err != nil {
		t.Fatal(err)
	}
	if string(tgt.files[nv]) != "source-has-new-boot-entry" {
		t.Errorf("target varstore = %q, want the source's", tgt.files[nv])
	}
}

func TestFirstSyncCopiesToAHostWithNoVarstore(t *testing.T) {
	src := &fakeHost{files: map[string][]byte{nv: []byte("source-varstore")}}
	tgt := &fakeHost{files: map[string][]byte{}}
	if err := syncNvram(context.Background(), src, tgt.writer("qemu:qemu"), xmlWithNvram, "web01", "web01"); err != nil {
		t.Fatal(err)
	}
	if string(tgt.files[nv]) != "source-varstore" {
		t.Error("the first sync did not install the varstore")
	}
}

// The XML names a varstore the source host does not have: report it, never
// silently write nothing.
func TestMissingSourceVarstoreIsReported(t *testing.T) {
	src := &fakeHost{files: map[string][]byte{}}
	tgt := &fakeHost{files: map[string][]byte{}}
	err := syncNvram(context.Background(), src, tgt.writer(""), xmlWithNvram, "web01", "web01")
	if err == nil {
		t.Fatal("a missing source varstore was not reported")
	}
	if !strings.Contains(err.Error(), nv) {
		t.Errorf("the error does not name the path: %v", err)
	}
}
