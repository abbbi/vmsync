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

package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock lets the lease tests assert on idle time without spending it.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// A RELAY NOBODY EVER CONNECTS TO MUST STILL GIVE UP.
//
// This is the case no pid file can rescue: the helper is started, its driver
// dies before connecting, and nothing on this host knows. Four such helpers
// were found listening on a production target two hours after the runs that
// started them were killed.
func TestRelayLeaseExpiresWhenNothingEverConnects(t *testing.T) {
	clk := &fakeClock{at: time.Unix(1_700_000_000, 0)}
	l := newRelayLease(10*time.Minute, clk.now)

	if l.expired() {
		t.Fatal("a relay that has just started is already expired")
	}
	clk.advance(9 * time.Minute)
	if l.expired() {
		t.Error("the lease expired before its time")
	}
	clk.advance(time.Minute)
	if !l.expired() {
		t.Error("nothing ever connected and the lease did not expire, so this relay would listen forever")
	}
}

// A LIVE CONNECTION SUSPENDS THE LEASE ENTIRELY.
//
// A full copy of a large disk holds one connection for hours. A lease that
// counted elapsed time rather than idle time would kill the sync it exists to
// carry, which would be a far worse bug than the one it fixes.
func TestRelayLeaseIsSuspendedByALiveConnection(t *testing.T) {
	clk := &fakeClock{at: time.Unix(1_700_000_000, 0)}
	l := newRelayLease(time.Minute, clk.now)

	l.begin()
	clk.advance(6 * time.Hour)
	if l.expired() {
		t.Error("the lease expired while a connection was open; a six-hour copy would be cut off by its own bridge")
	}

	// And it starts counting again from the moment the last one closes, not
	// from when the relay started.
	l.end()
	if l.expired() {
		t.Error("the lease expired the instant the connection closed, rather than giving it the full idle period")
	}
	clk.advance(59 * time.Second)
	if l.expired() {
		t.Error("the idle period is being measured from the wrong instant")
	}
	clk.advance(time.Second)
	if !l.expired() {
		t.Error("the connection closed and the lease never expired afterwards")
	}
}

// Several connections at once, which is what -io-depth produces.
func TestRelayLeaseWaitsForTheLastConnection(t *testing.T) {
	clk := &fakeClock{at: time.Unix(1_700_000_000, 0)}
	l := newRelayLease(time.Minute, clk.now)

	for i := 0; i < 8; i++ {
		l.begin()
	}
	for i := 0; i < 7; i++ {
		l.end()
	}
	clk.advance(time.Hour)
	if l.expired() {
		t.Error("the lease expired with one connection still open")
	}
	l.end()
	clk.advance(time.Minute)
	if !l.expired() {
		t.Error("the lease did not expire after the last connection closed")
	}
}

// drain is what stops expiry cutting a connection that arrived in the same
// instant the lease fired.
func TestRelayLeaseDrainWaitsForTheConnectionThatRacedExpiry(t *testing.T) {
	clk := &fakeClock{at: time.Unix(1_700_000_000, 0)}
	l := newRelayLease(time.Minute, clk.now)
	l.begin()

	done := make(chan struct{})
	go func() {
		l.drain()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("drain returned while a connection was still open, so serve would have exited underneath it")
	case <-time.After(100 * time.Millisecond):
	}

	l.end()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("drain never returned after the last connection closed")
	}
}

func TestRelayLeaseTickIsAThirdWithAFloor(t *testing.T) {
	if got := relayLeaseTick(15 * time.Minute); got != 5*time.Minute {
		t.Errorf("tick for a 15m lease = %s, want a third of it", got)
	}
	// A lease a test sets to a second must not make the watcher spin.
	if got := relayLeaseTick(time.Second); got < time.Second {
		t.Errorf("tick for a 1s lease = %s, want at least a second", got)
	}
	if got := relayLeaseTick(0); got < time.Second {
		t.Errorf("tick for a zero lease = %s, want at least a second", got)
	}
}

// THE HELPER RECORDS ITS OWN PID, which is the only way the record survives
// the shell that launched it.
func TestWriteOwnPIDFileRecordsThisProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmsync-bridge-test-20900.pid")

	if err := writeOwnPIDFile(path); err != nil {
		t.Fatalf("writeOwnPIDFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("recorded %q, which is not a pid: %v", b, err)
	}
	if got != os.Getpid() {
		t.Errorf("recorded pid %d, want this process's %d. BuildStopCommand kills the process GROUP of whatever is in here, so a wrong pid signals something else entirely", got, os.Getpid())
	}

	// No temporary left behind: the directory is read by vmsync's readiness
	// check and by any sweep, and a stray file there is a stray pid.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the pid directory holds %v, want only the pid file", names)
	}
}

// Replacing an existing file matters: a port can legitimately be reused by a
// later run, and a half-replaced pid file is a real pid belonging to
// something else.
func TestWriteOwnPIDFileReplacesAStaleRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.pid")
	if err := os.WriteFile(path, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeOwnPIDFile(path); err != nil {
		t.Fatalf("writeOwnPIDFile: %v", err)
	}
	b, _ := os.ReadFile(path)
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("stale record %q survived", b)
	}
}

func TestRelayStartupLineNamesWhatThisRelayIsDoing(t *testing.T) {
	cfg := helperConfig{ListenAddr: "0.0.0.0:20900", ConnectAddr: "127.0.0.1:20898"}

	got := relayStartupLine(cfg, 15*time.Minute)
	for _, want := range []string{"0.0.0.0:20900", "127.0.0.1:20898", strconv.Itoa(os.Getpid()), "15m"} {
		if !strings.Contains(got, want) {
			t.Errorf("startup line %q does not mention %q. Every leaked helper found so far had a zero-byte log, so this line is the only thing that outlives its driver", got, want)
		}
	}
	// And it says plainly when there is no lease, because that is the
	// configuration that can leak.
	if got := relayStartupLine(cfg, 0); !strings.Contains(got, "none") {
		t.Errorf("with no lease the startup line says %q, which does not say so", got)
	}
}

// freePort returns a port nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// serve END TO END: a relay nobody connects to ends itself, and takes its pid
// file with it.
func TestServeEndsItselfAndRemovesItsPIDFile(t *testing.T) {
	dir := t.TempDir()
	pid := filepath.Join(dir, "bridge.pid")
	cfg := helperConfig{
		ListenAddr:  freePort(t),
		ConnectAddr: freePort(t), // never dialled: nothing connects
		PIDFile:     pid,
		IdleLease:   time.Second,
	}

	done := make(chan error, 1)
	go func() { done <- serve(cfg) }()

	// It records itself while listening, which is what any cleanup path needs.
	deadline := time.Now().Add(5 * time.Second)
	var recorded string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pid); err == nil {
			recorded = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if recorded != strconv.Itoa(os.Getpid()) {
		t.Fatalf("while listening the pid file held %q, want %d", recorded, os.Getpid())
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve ended with %v; a lease expiring is an ordinary end, not a failure", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nothing connected and serve never returned: this is the leak")
	}

	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Errorf("the pid file outlived the relay (stat err = %v), so a sweep would chase a dead pid", err)
	}
}

// A relay with a connection open must NOT be ended by its lease, even long
// after the lease would otherwise have fired.
func TestServeKeepsRelayingWhileAConnectionIsOpen(t *testing.T) {
	// A backend that accepts and holds, so the relayed connection stays up.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	defer backend.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		accepted <- c
		// Reads until the relay half-closes, then closes -- which is what
		// qemu-nbd does when its client goes away, and what lets handleConn's
		// wg.Wait() return. A backend that held the connection open instead
		// would keep the lease suspended (see relayLease's note on that
		// limit), so modelling the real peer is the point here.
		io.Copy(io.Discard, c)
		c.Close()
	}()

	cfg := helperConfig{
		ListenAddr:  freePort(t),
		ConnectAddr: backend.Addr().String(),
		IdleLease:   time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- serve(cfg) }()

	// Connect before the lease can fire.
	var client net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", cfg.ListenAddr); err == nil {
			client = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("could not connect to the relay")
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay never dialled the backend")
	}

	// Well past the lease, with the connection still open.
	select {
	case err := <-done:
		t.Fatalf("serve returned (%v) while a connection was open: a long copy would be cut off by its own bridge", err)
	case <-time.After(4 * time.Second):
	}

	// Once it closes, the lease applies again.
	client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve ended with %v after the connection closed", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("the connection closed and the relay never ended itself")
	}
}

// Zero disables the lease, which has to remain possible and has to be the
// only way to get the old forever-listening behaviour.
func TestServeWithNoLeaseKeepsListening(t *testing.T) {
	cfg := helperConfig{
		ListenAddr:  freePort(t),
		ConnectAddr: freePort(t),
		IdleLease:   0,
	}
	done := make(chan error, 1)
	go func() { done <- serve(cfg) }()

	select {
	case err := <-done:
		t.Fatalf("serve returned (%v) with the lease disabled", err)
	case <-time.After(3 * time.Second):
	}
}

// A port already taken must fail loudly and leave NO pid file: the readiness
// check reads that file to decide whether the listener it found is the helper
// it just started, and a loser's record there would make a port collision
// look like a successful start.
func TestServeLeavesNoPIDFileWhenItCannotBind(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()

	dir := t.TempDir()
	pid := filepath.Join(dir, "loser.pid")
	err = serve(helperConfig{
		ListenAddr:  held.Addr().String(),
		ConnectAddr: freePort(t),
		PIDFile:     pid,
		IdleLease:   time.Minute,
	})
	if err == nil {
		t.Fatal("serve accepted a port that was already held")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error %q does not say the listen failed", err)
	}
	if _, statErr := os.Stat(pid); !os.IsNotExist(statErr) {
		t.Errorf("a helper that could not bind recorded itself anyway (stat err = %v)", statErr)
	}
}

func TestDefaultRelayLeaseIsGenerousButFinite(t *testing.T) {
	if defaultRelayLease <= 0 {
		t.Fatalf("the default lease is %s, which leaves every relay able to outlive its driver", defaultRelayLease)
	}
	// Long enough that no legitimate gap reaches it. vmsync starts a bridge
	// immediately before the export it fronts and connects within seconds.
	if defaultRelayLease < 5*time.Minute {
		t.Errorf("the default lease is %s, short enough to risk ending a relay a sync still wants", defaultRelayLease)
	}
	// Short enough that a leak does not outlive the day. The observed leak
	// sat for over two hours.
	if defaultRelayLease > time.Hour {
		t.Errorf("the default lease is %s, long enough that leaks still accumulate across a bench run", defaultRelayLease)
	}
	fmt.Fprintf(os.Stderr, "default relay lease: %s\n", defaultRelayLease)
}

// THE IDLE PERIOD IS MEASURED ON THE MONOTONIC CLOCK, so a clock step on the
// target cannot move it.
//
// time.Now carries both a wall reading and a monotonic one, and t2.Sub(t1)
// uses the monotonic pair when both operands have it -- but any value that
// has been through .UTC(), .Truncate(), .Round(0) or a marshalled form has
// had that reading stripped, and then the subtraction silently falls back to
// wall time. A relay is long-lived on a host whose clock an operator may fix
// mid-sync, and an NTP step backwards would otherwise postpone every expiry
// by the size of the step, or a step forwards expire a relay still in use.
//
// t.Round(0) is the documented way to strip the monotonic reading, so a Time
// that still carries one is not equal to its own Round(0).
func TestRelayLeaseMeasuresIdleOnTheMonotonicClock(t *testing.T) {
	l := newRelayLease(time.Minute, time.Now)

	l.mu.Lock()
	atStart := l.idleSince
	l.mu.Unlock()
	if atStart == atStart.Round(0) {
		t.Error("the idle clock set at construction carries no monotonic reading, so a clock step would move the idle measurement for a relay nothing ever connected to")
	}

	// And again on the path that restarts it, which is the one a real relay
	// actually takes.
	l.begin()
	l.end()
	l.mu.Lock()
	afterUse := l.idleSince
	l.mu.Unlock()
	if afterUse == afterUse.Round(0) {
		t.Error("the idle clock restarted when the last connection closed carries no monotonic reading")
	}
}
