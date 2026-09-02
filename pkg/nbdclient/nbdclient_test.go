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

package nbdclient

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests below run against a fake NBD server implemented here rather than
// against qemu-nbd, and that is the point: this package exists so the helper
// needs no libnbd, so its tests must not need a hypervisor either. What they
// can prove is that the bytes this client puts on the wire match the protocol
// document and that it reads replies back correctly, including the framing
// mistakes worth diagnosing. What they cannot prove is that qemu-nbd agrees;
// bench covers that end of it against a real export.

type fakeOpts struct {
	// data is the export's contents; its length is reported as the size.
	data []byte
	// wantExport, when non-empty, is the only export name accepted. Anything
	// else draws NBD_REP_ERR_UNKNOWN.
	wantExport string
	// oldstyle sends the export size where IHAVEOPT belongs, as a
	// pre-newstyle server would.
	oldstyle bool
	// noFixedNewstyle clears NBD_FLAG_FIXED_NEWSTYLE in the greeting.
	noFixedNewstyle bool
	// readErr, when nonzero, is returned as the error code for every read.
	readErr uint32
	// badReplyMagic answers reads with the structured-reply magic, which
	// this client never negotiates and so must reject.
	badReplyMagic bool
	// wrongCookie echoes a cookie the client did not send.
	wrongCookie bool
	// omitInfoExport acknowledges NBD_OPT_GO without ever sending
	// NBD_INFO_EXPORT, leaving the client with no size.
	omitInfoExport bool
	// extraInfo sends an NBD_INFO_NAME reply before NBD_INFO_EXPORT, which
	// a client must skip rather than refuse.
	extraInfo bool

	// gotExport records the export name the client actually asked for.
	gotExport chan string
	// gotReads records each read request as "offset:length".
	gotReads chan string
}

func startFake(t *testing.T, o *fakeOpts) string {
	t.Helper()
	if o.gotExport == nil {
		o.gotExport = make(chan string, 4)
	}
	if o.gotReads == nil {
		o.gotReads = make(chan string, 64)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFake(conn, o)
		}
	}()
	return ln.Addr().String()
}

func serveFake(conn net.Conn, o *fakeOpts) {
	defer conn.Close()

	greeting := make([]byte, 0, 18)
	greeting = binary.BigEndian.AppendUint64(greeting, magicNBD)
	if o.oldstyle {
		greeting = binary.BigEndian.AppendUint64(greeting, uint64(len(o.data)))
	} else {
		greeting = binary.BigEndian.AppendUint64(greeting, magicIHaveOpt)
	}
	flags := flagFixedNewstyle | flagNoZeroes
	if o.noFixedNewstyle {
		flags = flagNoZeroes
	}
	greeting = binary.BigEndian.AppendUint16(greeting, flags)
	if _, err := conn.Write(greeting); err != nil {
		return
	}
	if o.oldstyle || o.noFixedNewstyle {
		return // the client must give up here
	}

	var cf [4]byte
	if _, err := io.ReadFull(conn, cf[:]); err != nil {
		return
	}

	// NBD_OPT_GO
	var oh [16]byte
	if _, err := io.ReadFull(conn, oh[:]); err != nil {
		return
	}
	optLen := binary.BigEndian.Uint32(oh[12:16])
	body := make([]byte, optLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return
	}
	nameLen := binary.BigEndian.Uint32(body[0:4])
	export := string(body[4 : 4+nameLen])
	o.gotExport <- export

	if o.wantExport != "" && export != o.wantExport {
		writeOptReply(conn, repErrorBit|6, []byte("no such export"))
		return
	}

	if o.extraInfo {
		// NBD_INFO_NAME (1) -- must be skipped, not refused.
		info := binary.BigEndian.AppendUint16(nil, 1)
		info = append(info, "somename"...)
		writeOptReply(conn, repInfo, info)
	}
	if !o.omitInfoExport {
		info := binary.BigEndian.AppendUint16(nil, infoExport)
		info = binary.BigEndian.AppendUint64(info, uint64(len(o.data)))
		info = binary.BigEndian.AppendUint16(info, 0)
		writeOptReply(conn, repInfo, info)
	}
	writeOptReply(conn, repAck, nil)

	for {
		var req [28]byte
		if _, err := io.ReadFull(conn, req[:]); err != nil {
			return
		}
		if binary.BigEndian.Uint32(req[0:4]) != magicRequest {
			return
		}
		cmd := binary.BigEndian.Uint16(req[6:8])
		cookie := binary.BigEndian.Uint64(req[8:16])
		off := binary.BigEndian.Uint64(req[16:24])
		length := binary.BigEndian.Uint32(req[24:28])
		if cmd == cmdDisc {
			return
		}
		if cmd != cmdRead {
			return
		}
		o.gotReads <- fmt.Sprintf("%d:%d", off, length)

		magic := magicSimple
		if o.badReplyMagic {
			magic = 0x668e33ef // NBD_STRUCTURED_REPLY_MAGIC
		}
		replyCookie := cookie
		if o.wrongCookie {
			replyCookie = cookie + 1000
		}
		rep := make([]byte, 0, 16)
		rep = binary.BigEndian.AppendUint32(rep, magic)
		rep = binary.BigEndian.AppendUint32(rep, o.readErr)
		rep = binary.BigEndian.AppendUint64(rep, replyCookie)
		if _, err := conn.Write(rep); err != nil {
			return
		}
		if o.readErr != 0 || o.badReplyMagic {
			continue
		}
		if _, err := conn.Write(o.data[off : off+uint64(length)]); err != nil {
			return
		}
	}
}

func writeOptReply(conn net.Conn, replyType uint32, payload []byte) {
	hdr := make([]byte, 0, 20)
	hdr = binary.BigEndian.AppendUint64(hdr, magicRep)
	hdr = binary.BigEndian.AppendUint32(hdr, optGo)
	hdr = binary.BigEndian.AppendUint32(hdr, replyType)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(payload)))
	if _, err := conn.Write(hdr); err != nil {
		return
	}
	if len(payload) > 0 {
		_, _ = conn.Write(payload)
	}
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func dialFake(t *testing.T, o *fakeOpts, export string) *Client {
	t.Helper()
	addr := startFake(t, o)
	c, err := Dial(context.Background(), addr, export, 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDialNegotiatesNamedExportAndReportsSize(t *testing.T) {
	o := &fakeOpts{data: pattern(4096), wantExport: "vm-vda"}
	c := dialFake(t, o, "vm-vda")

	if got := c.Size(); got != 4096 {
		t.Errorf("Size() = %d, want 4096", got)
	}
	select {
	case name := <-o.gotExport:
		if name != "vm-vda" {
			t.Errorf("server saw export %q, want %q", name, "vm-vda")
		}
	default:
		t.Fatal("server recorded no export name")
	}
}

func TestReadAtReturnsExportContents(t *testing.T) {
	data := pattern(64 << 10)
	c := dialFake(t, &fakeOpts{data: data}, "e")

	for _, tc := range []struct{ off, n int }{
		{0, 1},
		{0, 512},
		{4096, 4096},
		{(64 << 10) - 10, 10},
		{1, 4095}, // deliberately unaligned on both ends
	} {
		buf := make([]byte, tc.n)
		if err := c.ReadAt(buf, uint64(tc.off)); err != nil {
			t.Fatalf("ReadAt(%d,%d): %v", tc.off, tc.n, err)
		}
		if string(buf) != string(data[tc.off:tc.off+tc.n]) {
			t.Errorf("ReadAt(%d,%d) returned wrong bytes", tc.off, tc.n)
		}
	}
}

// A read longer than maxRequestLength must be split, because qemu-nbd
// enforces a ceiling on a single request. The point of the test is the
// splitting, so it checks the request sizes the server actually saw.
func TestReadAtSplitsOversizedRequests(t *testing.T) {
	const total = maxRequestLength + 4096
	data := pattern(total)
	o := &fakeOpts{data: data}
	c := dialFake(t, o, "e")

	buf := make([]byte, total)
	if err := c.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != string(data) {
		t.Fatal("split read reassembled wrong bytes")
	}

	var got []string
	for {
		select {
		case r := <-o.gotReads:
			got = append(got, r)
			continue
		default:
		}
		break
	}
	want := []string{
		fmt.Sprintf("0:%d", maxRequestLength),
		fmt.Sprintf("%d:4096", maxRequestLength),
	}
	if len(got) != len(want) {
		t.Fatalf("server saw %d requests %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestReadAtEmptyIsNoop(t *testing.T) {
	o := &fakeOpts{data: pattern(4096)}
	c := dialFake(t, o, "e")
	if err := c.ReadAt(nil, 0); err != nil {
		t.Fatalf("ReadAt(nil): %v", err)
	}
	select {
	case r := <-o.gotReads:
		t.Errorf("empty read still sent a request: %s", r)
	default:
	}
}

// Past-the-end reads are refused locally. A server would answer EINVAL, but
// the local message can name the export size, which the errno cannot.
func TestReadAtPastEndIsRefusedWithoutSendingARequest(t *testing.T) {
	o := &fakeOpts{data: pattern(4096)}
	c := dialFake(t, o, "e")

	buf := make([]byte, 8)
	err := c.ReadAt(buf, 4090)
	if err == nil {
		t.Fatal("ReadAt past end succeeded, want error")
	}
	if !strings.Contains(err.Error(), "past export size") {
		t.Errorf("error = %v, want it to mention the export size", err)
	}
	select {
	case r := <-o.gotReads:
		t.Errorf("past-end read was sent to the server anyway: %s", r)
	default:
	}
}

func TestReadAtOffsetOverflowIsRefused(t *testing.T) {
	c := dialFake(t, &fakeOpts{data: pattern(4096)}, "e")
	err := c.ReadAt(make([]byte, 16), ^uint64(0)-4)
	if err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Errorf("err = %v, want an overflow error", err)
	}
}

func TestUnknownExportIsTyped(t *testing.T) {
	addr := startFake(t, &fakeOpts{data: pattern(4096), wantExport: "right"})
	_, err := Dial(context.Background(), addr, "wrong", 5*time.Second)
	if err == nil {
		t.Fatal("Dial with a wrong export name succeeded")
	}
	if !errors.Is(err, ErrExportNotFound) {
		t.Errorf("err = %v, want ErrExportNotFound", err)
	}
	// The name asked for belongs in the message: the failure mode this
	// exists for is connecting to the right port and the wrong export.
	if !strings.Contains(err.Error(), "wrong") {
		t.Errorf("err = %v, want it to name the requested export", err)
	}
}

func TestServerReadErrorMapsToErrno(t *testing.T) {
	c := dialFake(t, &fakeOpts{data: pattern(4096), readErr: 5}, "e")
	err := c.ReadAt(make([]byte, 512), 0)
	if err == nil {
		t.Fatal("read succeeded despite a server error")
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("err = %v, want it to wrap syscall.EIO", err)
	}
}

// Structured replies are client-opt-in and this client never asks, so a
// structured reply means the server is doing something unnegotiated. The
// message must say which magic arrived -- that is the entire diagnosis.
func TestStructuredReplyMagicIsRejectedClearly(t *testing.T) {
	c := dialFake(t, &fakeOpts{data: pattern(4096), badReplyMagic: true}, "e")
	err := c.ReadAt(make([]byte, 512), 0)
	if err == nil {
		t.Fatal("read succeeded despite an unnegotiated reply format")
	}
	if !strings.Contains(err.Error(), "0x668e33ef") {
		t.Errorf("err = %v, want it to name the magic that arrived", err)
	}
}

func TestCookieMismatchIsDetected(t *testing.T) {
	c := dialFake(t, &fakeOpts{data: pattern(4096), wrongCookie: true}, "e")
	err := c.ReadAt(make([]byte, 512), 0)
	if err == nil || !strings.Contains(err.Error(), "cookie mismatch") {
		t.Errorf("err = %v, want a cookie mismatch error", err)
	}
}

func TestOldstyleServerIsRefused(t *testing.T) {
	addr := startFake(t, &fakeOpts{data: pattern(4096), oldstyle: true})
	_, err := Dial(context.Background(), addr, "e", 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "newstyle") {
		t.Errorf("err = %v, want a newstyle negotiation error", err)
	}
}

func TestMissingFixedNewstyleFlagIsRefused(t *testing.T) {
	addr := startFake(t, &fakeOpts{data: pattern(4096), noFixedNewstyle: true})
	_, err := Dial(context.Background(), addr, "e", 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "fixed newstyle") {
		t.Errorf("err = %v, want a fixed-newstyle error", err)
	}
}

// An ACK with no NBD_INFO_EXPORT would leave size at zero, and every
// subsequent ReadAt would then fail its own bounds check with a confusing
// "past export size 0". Fail at the handshake instead, where it is legible.
func TestAckWithoutInfoExportIsRefused(t *testing.T) {
	addr := startFake(t, &fakeOpts{data: pattern(4096), omitInfoExport: true})
	_, err := Dial(context.Background(), addr, "e", 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "NBD_INFO_EXPORT") {
		t.Errorf("err = %v, want a missing-NBD_INFO_EXPORT error", err)
	}
}

func TestUnrelatedInfoRepliesAreSkipped(t *testing.T) {
	c := dialFake(t, &fakeOpts{data: pattern(2048), extraInfo: true}, "e")
	if got := c.Size(); got != 2048 {
		t.Errorf("Size() = %d, want 2048 (NBD_INFO_NAME should have been skipped)", got)
	}
}

func TestEmptyExportNameAsksForTheDefault(t *testing.T) {
	o := &fakeOpts{data: pattern(1024)}
	c := dialFake(t, o, "")
	if got := c.Size(); got != 1024 {
		t.Errorf("Size() = %d, want 1024", got)
	}
	select {
	case name := <-o.gotExport:
		if name != "" {
			t.Errorf("server saw export %q, want the empty default", name)
		}
	default:
		t.Fatal("server recorded no export name")
	}
}

func TestDialRespectsCancelledContext(t *testing.T) {
	addr := startFake(t, &fakeOpts{data: pattern(1024)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Dial(ctx, addr, "e", 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestDialFailsOnClosedPort(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly unused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if _, err := Dial(context.Background(), addr, "e", 2*time.Second); err == nil {
		t.Fatal("Dial to a closed port succeeded")
	}
}

func TestCloseSendsDisconnect(t *testing.T) {
	o := &fakeOpts{data: pattern(1024)}
	addr := startFake(t, o)
	c, err := Dial(context.Background(), addr, "e", 5*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close is idempotent enough not to panic; the second one errors on the
	// already-closed socket, which is fine and must not be a panic.
	_ = c.Close()
}
