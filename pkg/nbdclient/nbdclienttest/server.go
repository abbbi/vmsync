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

// Package nbdclienttest provides a small, in-process, read-only NBD server
// for tests.
//
// It serves the correct path only -- a well-behaved fixed-newstyle server
// over a byte slice -- so that a test which needs "something to read from"
// does not have to reimplement the handshake. pkg/nbdclient's own tests
// keep a separate, deliberately misbehaving server for their fault
// injection; this one exists for everything downstream of the client, where
// the protocol is not what is under test.
//
// Its reason for existing outside a _test.go file is that the code needing
// it lives in other packages: cmd/vmsync-bridge-helper's checksum mode is
// package main, so it cannot import an external test package, and a fake in
// each consumer would be the same handshake written several times over.
package nbdclienttest

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

// Protocol constants. Duplicated from pkg/nbdclient rather than exported
// from it: they are unexported there deliberately (a minimal client should
// not publish a protocol surface it does not implement), and a fake server
// that shares the client's own constants could agree with it about a value
// that is wrong in both. Written out independently, the two have to match
// the specification rather than each other.
const (
	magicNBD      uint64 = 0x4e42444d41474943 // "NBDMAGIC"
	magicIHaveOpt uint64 = 0x49484156454f5054 // "IHAVEOPT"
	magicRep      uint64 = 0x0003e889045565a9
	magicRequest  uint32 = 0x25609513
	magicSimple   uint32 = 0x67446698

	flagFixedNewstyle uint16 = 1 << 0
	flagNoZeroes      uint16 = 1 << 1

	optGo uint32 = 7

	repAck        uint32 = 1
	repInfo       uint32 = 3
	repErrUnknown uint32 = 1<<31 | 6

	infoExport uint16 = 0

	cmdRead uint16 = 0
	cmdDisc uint16 = 2
)

// Server is a running fake NBD server. Close it when done.
type Server struct {
	ln     net.Listener
	data   []byte
	export string

	mu    sync.Mutex
	reads int
}

// NewServer starts a read-only NBD server exporting data under the name
// export, listening on an ephemeral loopback port.
//
// An empty export accepts any name the client asks for. Otherwise the name
// must match exactly, and a mismatch draws NBD_REP_ERR_UNKNOWN -- which is
// what lets a caller test that connecting to the wrong export fails rather
// than silently reading the wrong disk.
func NewServer(data []byte, export string) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, data: data, export: export}
	go s.acceptLoop()
	return s, nil
}

// Addr is the host:port to hand to a client.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Reads is how many NBD_CMD_READ requests have been served, which lets a
// test assert that a pass which should have skipped the export entirely did
// not connect and read anyway.
func (s *Server) Reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// Close stops accepting new connections.
func (s *Server) Close() { _ = s.ln.Close() }

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = s.serve(conn)
		}()
	}
}

func (s *Server) serve(conn net.Conn) error {
	greeting := make([]byte, 0, 18)
	greeting = binary.BigEndian.AppendUint64(greeting, magicNBD)
	greeting = binary.BigEndian.AppendUint64(greeting, magicIHaveOpt)
	greeting = binary.BigEndian.AppendUint16(greeting, flagFixedNewstyle|flagNoZeroes)
	if _, err := conn.Write(greeting); err != nil {
		return err
	}

	var clientFlags [4]byte
	if _, err := io.ReadFull(conn, clientFlags[:]); err != nil {
		return err
	}

	var optHdr [16]byte
	if _, err := io.ReadFull(conn, optHdr[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint64(optHdr[0:8]) != magicIHaveOpt {
		return errors.New("bad option magic")
	}
	if binary.BigEndian.Uint32(optHdr[8:12]) != optGo {
		return errors.New("only NBD_OPT_GO is implemented")
	}
	body := make([]byte, binary.BigEndian.Uint32(optHdr[12:16]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return err
	}
	if len(body) < 4 {
		return errors.New("short NBD_OPT_GO body")
	}
	nameLen := binary.BigEndian.Uint32(body[0:4])
	if uint64(nameLen)+4 > uint64(len(body)) {
		return errors.New("NBD_OPT_GO name runs past the body")
	}
	asked := string(body[4 : 4+nameLen])
	if s.export != "" && asked != s.export {
		return writeOptReply(conn, repErrUnknown, []byte("no such export"))
	}

	info := binary.BigEndian.AppendUint16(nil, infoExport)
	info = binary.BigEndian.AppendUint64(info, uint64(len(s.data)))
	info = binary.BigEndian.AppendUint16(info, 0)
	if err := writeOptReply(conn, repInfo, info); err != nil {
		return err
	}
	if err := writeOptReply(conn, repAck, nil); err != nil {
		return err
	}

	for {
		var req [28]byte
		if _, err := io.ReadFull(conn, req[:]); err != nil {
			return nil // client hung up
		}
		if binary.BigEndian.Uint32(req[0:4]) != magicRequest {
			return errors.New("bad request magic")
		}
		cmd := binary.BigEndian.Uint16(req[6:8])
		cookie := binary.BigEndian.Uint64(req[8:16])
		off := binary.BigEndian.Uint64(req[16:24])
		length := uint64(binary.BigEndian.Uint32(req[24:28]))

		if cmd == cmdDisc {
			return nil
		}
		if cmd != cmdRead {
			if err := writeSimpleReply(conn, 22, cookie, nil); err != nil { // EINVAL
				return err
			}
			continue
		}
		if off+length < off || off+length > uint64(len(s.data)) {
			if err := writeSimpleReply(conn, 22, cookie, nil); err != nil { // EINVAL
				return err
			}
			continue
		}
		s.mu.Lock()
		s.reads++
		s.mu.Unlock()
		if err := writeSimpleReply(conn, 0, cookie, s.data[off:off+length]); err != nil {
			return err
		}
	}
}

func writeOptReply(conn net.Conn, replyType uint32, payload []byte) error {
	hdr := make([]byte, 0, 20+len(payload))
	hdr = binary.BigEndian.AppendUint64(hdr, magicRep)
	hdr = binary.BigEndian.AppendUint32(hdr, optGo)
	hdr = binary.BigEndian.AppendUint32(hdr, replyType)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(payload)))
	hdr = append(hdr, payload...)
	_, err := conn.Write(hdr)
	return err
}

func writeSimpleReply(conn net.Conn, errCode uint32, cookie uint64, payload []byte) error {
	rep := make([]byte, 0, 16+len(payload))
	rep = binary.BigEndian.AppendUint32(rep, magicSimple)
	rep = binary.BigEndian.AppendUint32(rep, errCode)
	rep = binary.BigEndian.AppendUint64(rep, cookie)
	rep = append(rep, payload...)
	_, err := conn.Write(rep)
	return err
}
