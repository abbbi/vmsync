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

// Package nbdclient is a minimal, read-only NBD client speaking the fixed
// newstyle handshake, in pure Go.
//
// It exists so vmsync-bridge-helper can read a target disk's guest-visible
// content -- the bytes a qcow2 export actually serves, flattened across any
// backing chain -- without gaining a cgo dependency. The helper is deployed
// to every target host in an estate, and libnbd (which cmd/vmsync itself
// links) would make that deployment a compiled-dependency problem rather
// than a copy-one-static-binary problem. Hashing the qcow2 FILE is not an
// alternative: two qcow2 images with identical guest content differ
// byte-for-byte, so only a format-aware read answers the question.
//
// Deliberately not a general NBD implementation. The scope is exactly what
// the checksum path needs:
//
//   - fixed newstyle handshake with NBD_OPT_GO and a named export
//   - NBD_CMD_READ
//   - NBD_CMD_DISC
//
// Everything optional is left un-negotiated, and that is a simplification
// with teeth: structured replies, extended headers and TLS are all
// client-opt-in, so a server only uses them if asked. By never asking, the
// reply framing stays the fixed 16-byte simple reply and there is no variant
// to parse. Writes, trim, block-status and flush are absent because a
// checksum pass never issues them -- an export this connects to may well be
// read-only anyway.
//
// The connection is single-threaded by construction: one request in flight,
// its reply read before the next is sent. NBD allows pipelining and
// cmd/vmsync's libnbd-based copy path uses it for throughput, but here the
// work is bounded by hashing and local disk reads on the target rather than
// by round-trip latency over a loopback socket, so the concurrency would buy
// little and the cookie-matching machinery it needs is exactly the kind of
// thing a minimal client should not carry.
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
	"time"
)

// Protocol constants, from the NBD protocol document ("fixed newstyle
// negotiation" and "transmission phase"). Named exactly as the spec names
// them so the two can be read side by side.
const (
	magicNBD      uint64 = 0x4e42444d41474943 // "NBDMAGIC"
	magicIHaveOpt uint64 = 0x49484156454f5054 // "IHAVEOPT"
	magicRep      uint64 = 0x0003e889045565a9
	magicRequest  uint32 = 0x25609513
	magicSimple   uint32 = 0x67446698

	// Handshake flags, sent by the server.
	flagFixedNewstyle uint16 = 1 << 0
	flagNoZeroes      uint16 = 1 << 1

	// Client flags, sent in reply.
	clientFlagFixedNewstyle uint32 = 1 << 0
	clientFlagNoZeroes      uint32 = 1 << 1

	optGo uint32 = 7

	repAck  uint32 = 1
	repInfo uint32 = 3
	// Error replies have the high bit set; the low bits say which error.
	repErrorBit uint32 = 1 << 31

	infoExport uint16 = 0

	cmdRead uint16 = 0
	cmdDisc uint16 = 2
)

// maxRequestLength caps a single NBD_CMD_READ. The protocol document advises
// clients not to exceed 32 MiB without negotiating a larger maximum, and
// qemu-nbd enforces that ceiling; 4 MiB stays well inside it while keeping
// the per-request overhead irrelevant next to the read itself. ReadAt splits
// anything larger, so callers never have to know this number.
const maxRequestLength = 4 << 20

// ErrExportNotFound reports that the server refused the requested export
// name. Distinguished from a transport failure because it is the signature
// of the one mistake worth naming precisely: connecting to the right port
// but the wrong export -- a stale qemu-nbd from an earlier run still holding
// the port, or two runs colliding on it.
var ErrExportNotFound = errors.New("nbd export not found")

// Client is a connected, read-only NBD session. Not safe for concurrent use:
// see the package doc on why that is deliberate.
type Client struct {
	conn    net.Conn
	size    uint64
	cookie  uint64
	timeout time.Duration
}

// networkFor picks the transport from the shape of addr: a leading "/" (a
// filesystem path) or "@" (a Linux abstract socket) means a Unix socket,
// anything else is TCP host:port.
//
// A heuristic, but an unambiguous one -- a TCP address is host:port and can
// never begin with either character -- and it keeps callers, including the
// helper's own -nbd flag, to a single address argument instead of a flag
// pair plus the rule for which one wins.
func networkFor(addr string) string {
	if strings.HasPrefix(addr, "/") || strings.HasPrefix(addr, "@") {
		return "unix"
	}
	return "tcp"
}

// Dial connects to addr, negotiates the named export and returns a client
// positioned in the transmission phase.
//
// addr is either a TCP host:port or a Unix socket path (see networkFor).
// The Unix case is the one the pre-commit integrity check uses: that export
// exists solely to be read by this process on the same host, so binding it
// to a TCP port would spend a port from the run's reservation and publish an
// export full of guest data on the network for no reason at all.
//
// export may be empty, which asks for the server's default export. Every
// export vmsync creates is named (see targetExportName in cmd/vmsync), and
// naming is a safety property rather than tidiness -- an address says only
// that something is listening, a name says it is the export that was meant
// -- so callers inside vmsync should always pass one.
//
// timeout bounds each individual socket operation, not the call as a whole:
// a checksum pass reads far more than one round trip, and a deadline over
// the whole session would fail a large but healthy read. Cancel ctx to abort
// the session as a whole.
func Dial(ctx context.Context, addr, export string, timeout time.Duration) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, networkFor(addr), addr)
	if err != nil {
		return nil, fmt.Errorf("dial nbd %s: %w", addr, err)
	}
	c := &Client{conn: conn, timeout: timeout}

	// Abort the blocking socket calls below when ctx is cancelled. Closing
	// the connection is the only way to interrupt them -- net.Conn has no
	// context-aware Read/Write -- and it makes the in-flight call return a
	// "use of closed network connection" error rather than hanging until its
	// deadline. done stops the goroutine on the success path so it cannot
	// outlive the handshake and close a connection the caller now owns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if err := c.handshake(export); err != nil {
		_ = conn.Close()
		// Prefer ctx's own error: a cancelled context closes the connection
		// out from under the handshake, and the resulting "closed network
		// connection" would otherwise be reported as a protocol failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return c, nil
}

// Size is the export's length in bytes, as reported by NBD_INFO_EXPORT.
func (c *Client) Size() uint64 { return c.size }

// Close sends NBD_CMD_DISC and closes the connection.
//
// The disconnect is best-effort: a server that has already gone away leaves
// nothing to tell, and the connection is closed either way. Only the close
// error is returned, and only when the disconnect itself succeeded, so a
// caller checking Close still learns about a socket that failed to shut down
// cleanly without being told about an unremarkable racing teardown.
func (c *Client) Close() error {
	var buf [28]byte
	binary.BigEndian.PutUint32(buf[0:4], magicRequest)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	binary.BigEndian.PutUint16(buf[6:8], cmdDisc)
	binary.BigEndian.PutUint64(buf[8:16], c.nextCookie())
	// offset and length stay zero for NBD_CMD_DISC.
	discErr := c.write(buf[:])
	closeErr := c.conn.Close()
	if discErr != nil {
		return closeErr
	}
	return closeErr
}

// ReadAt fills p with the export's contents starting at off.
//
// Reads longer than maxRequestLength are split transparently. A read running
// past the end of the export is refused locally rather than sent: NBD servers
// answer that with EINVAL, and the local check produces a message naming the
// actual offsets instead.
func (c *Client) ReadAt(p []byte, off uint64) error {
	if len(p) == 0 {
		return nil
	}
	end := off + uint64(len(p))
	if end < off {
		return fmt.Errorf("nbd read at %d length %d overflows", off, len(p))
	}
	if end > c.size {
		return fmt.Errorf("nbd read at %d length %d runs past export size %d", off, len(p), c.size)
	}
	for len(p) > 0 {
		n := len(p)
		if n > maxRequestLength {
			n = maxRequestLength
		}
		if err := c.readOnce(p[:n], off); err != nil {
			return err
		}
		p = p[n:]
		off += uint64(n)
	}
	return nil
}

func (c *Client) readOnce(p []byte, off uint64) error {
	cookie := c.nextCookie()

	var req [28]byte
	binary.BigEndian.PutUint32(req[0:4], magicRequest)
	binary.BigEndian.PutUint16(req[4:6], 0)
	binary.BigEndian.PutUint16(req[6:8], cmdRead)
	binary.BigEndian.PutUint64(req[8:16], cookie)
	binary.BigEndian.PutUint64(req[16:24], off)
	binary.BigEndian.PutUint32(req[24:28], uint32(len(p)))
	if err := c.write(req[:]); err != nil {
		return fmt.Errorf("nbd read request at %d: %w", off, err)
	}

	var rep [16]byte
	if err := c.read(rep[:]); err != nil {
		return fmt.Errorf("nbd read reply header at %d: %w", off, err)
	}
	if got := binary.BigEndian.Uint32(rep[0:4]); got != magicSimple {
		// A structured reply here would mean the server used a feature this
		// client never negotiated, so say which magic arrived rather than
		// just "bad magic" -- that distinction is the whole diagnosis.
		return fmt.Errorf("nbd read reply at %d: unexpected reply magic 0x%08x (want simple reply 0x%08x)", off, got, magicSimple)
	}
	if errCode := binary.BigEndian.Uint32(rep[4:8]); errCode != 0 {
		return fmt.Errorf("nbd read at %d length %d: server error %d (%w)", off, len(p), errCode, errnoToError(errCode))
	}
	if got := binary.BigEndian.Uint64(rep[8:16]); got != cookie {
		return fmt.Errorf("nbd read reply at %d: cookie mismatch (got %d, want %d)", off, got, cookie)
	}
	if err := c.read(p); err != nil {
		return fmt.Errorf("nbd read payload at %d length %d: %w", off, len(p), err)
	}
	return nil
}

func (c *Client) nextCookie() uint64 {
	c.cookie++
	return c.cookie
}

func (c *Client) handshake(export string) error {
	var hello [18]byte
	if err := c.read(hello[:]); err != nil {
		return fmt.Errorf("read nbd greeting: %w", err)
	}
	if got := binary.BigEndian.Uint64(hello[0:8]); got != magicNBD {
		return fmt.Errorf("not an nbd server: greeting magic 0x%016x", got)
	}
	if got := binary.BigEndian.Uint64(hello[8:16]); got != magicIHaveOpt {
		// The oldstyle protocol sends the export size here instead. Worth
		// naming: it means the server predates newstyle negotiation, which
		// no qemu-nbd this code will meet does.
		return fmt.Errorf("nbd server does not offer newstyle negotiation (magic 0x%016x)", got)
	}
	serverFlags := binary.BigEndian.Uint16(hello[16:18])
	if serverFlags&flagFixedNewstyle == 0 {
		return errors.New("nbd server does not support fixed newstyle negotiation")
	}

	clientFlags := clientFlagFixedNewstyle
	if serverFlags&flagNoZeroes != 0 {
		clientFlags |= clientFlagNoZeroes
	}
	var cf [4]byte
	binary.BigEndian.PutUint32(cf[:], clientFlags)
	if err := c.write(cf[:]); err != nil {
		return fmt.Errorf("send nbd client flags: %w", err)
	}

	// NBD_OPT_GO: export name, then a count of additional info requests.
	// Zero requests -- NBD_INFO_EXPORT comes back regardless, and it carries
	// the only field this client needs.
	data := make([]byte, 0, 4+len(export)+2)
	data = binary.BigEndian.AppendUint32(data, uint32(len(export)))
	data = append(data, export...)
	data = binary.BigEndian.AppendUint16(data, 0)

	opt := make([]byte, 0, 16+len(data))
	opt = binary.BigEndian.AppendUint64(opt, magicIHaveOpt)
	opt = binary.BigEndian.AppendUint32(opt, optGo)
	opt = binary.BigEndian.AppendUint32(opt, uint32(len(data)))
	opt = append(opt, data...)
	if err := c.write(opt); err != nil {
		return fmt.Errorf("send NBD_OPT_GO: %w", err)
	}

	// The server answers with any number of NBD_REP_INFO replies followed by
	// NBD_REP_ACK, which is what moves the session into transmission.
	haveSize := false
	for {
		var hdr [20]byte
		if err := c.read(hdr[:]); err != nil {
			return fmt.Errorf("read NBD_OPT_GO reply: %w", err)
		}
		if got := binary.BigEndian.Uint64(hdr[0:8]); got != magicRep {
			return fmt.Errorf("bad option reply magic 0x%016x", got)
		}
		if got := binary.BigEndian.Uint32(hdr[8:12]); got != optGo {
			return fmt.Errorf("option reply for %d, expected NBD_OPT_GO (%d)", got, optGo)
		}
		replyType := binary.BigEndian.Uint32(hdr[12:16])
		length := binary.BigEndian.Uint32(hdr[16:20])
		if length > 1<<20 {
			return fmt.Errorf("option reply payload of %d bytes is implausible", length)
		}
		payload := make([]byte, length)
		if err := c.read(payload); err != nil {
			return fmt.Errorf("read option reply payload: %w", err)
		}

		switch {
		case replyType == repAck:
			if !haveSize {
				return errors.New("nbd server acknowledged NBD_OPT_GO without sending NBD_INFO_EXPORT")
			}
			return nil
		case replyType == repInfo:
			// NBD_INFO_EXPORT: info type, 8-byte size, 2-byte transmission
			// flags. Other info types are skipped rather than refused --
			// a server may send NBD_INFO_NAME or NBD_INFO_BLOCK_SIZE
			// unprompted, and none of them are errors.
			if len(payload) >= 10 && binary.BigEndian.Uint16(payload[0:2]) == infoExport {
				c.size = binary.BigEndian.Uint64(payload[2:10])
				haveSize = true
			}
		case replyType&repErrorBit != 0:
			// The payload is an optional human-readable string.
			msg := string(payload)
			// 0x80000006 is NBD_REP_ERR_UNKNOWN: "the export this client
			// asked for does not exist", which is worth a typed error.
			if replyType == repErrorBit|6 {
				if msg != "" {
					return fmt.Errorf("%w: %q: %s", ErrExportNotFound, export, msg)
				}
				return fmt.Errorf("%w: %q", ErrExportNotFound, export)
			}
			if msg != "" {
				return fmt.Errorf("nbd server refused NBD_OPT_GO: error 0x%08x: %s", replyType, msg)
			}
			return fmt.Errorf("nbd server refused NBD_OPT_GO: error 0x%08x", replyType)
		default:
			return fmt.Errorf("unexpected NBD_OPT_GO reply type %d", replyType)
		}
	}
}

func (c *Client) write(p []byte) error {
	if c.timeout > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return err
		}
	}
	_, err := c.conn.Write(p)
	return err
}

func (c *Client) read(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if c.timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			return err
		}
	}
	_, err := io.ReadFull(c.conn, p)
	return err
}

// errnoToError maps an NBD error code to a Go error. NBD reuses Unix errno
// values, and wrapping them keeps errors.Is(err, syscall.EIO) working for a
// caller that wants to tell a read error apart from a permission problem.
func errnoToError(code uint32) error {
	switch code {
	case 1:
		return syscall.EPERM
	case 5:
		return syscall.EIO
	case 12:
		return syscall.ENOMEM
	case 22:
		return syscall.EINVAL
	case 28:
		return syscall.ENOSPC
	case 75:
		return syscall.EOVERFLOW
	case 95:
		return syscall.ENOTSUP
	case 108:
		return errors.New("server is shutting down")
	default:
		return fmt.Errorf("nbd error code %d", code)
	}
}
