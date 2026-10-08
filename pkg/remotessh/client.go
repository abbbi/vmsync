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

package remotessh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"vmsync/pkg/trace"
)

type Config struct {
	Address               string
	Port                  int
	User                  string
	PrivateKeyPath        string
	Password              string
	InsecureIgnoreHostKey bool
	KnownHostsPath        string
	Timeout               time.Duration
}

type Client struct {
	cfg    Config
	client *ssh.Client
	keepaliveState
}

// LoopbackSelfAddress returns 127.0.0.1:<ssh-port> for this client's remote
// host -- a destination sshd itself is always listening on, usable as a
// self-test direct-tcpip dial target without depending on hostname
// resolution working the same way from the remote side as it did locally.
func (c *Client) LoopbackSelfAddress() string {
	return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", c.cfg.Port))
}

// ConfigFromLibvirtURI builds a Config from a libvirt connection URI
// (e.g. qemu+ssh://alias/system), consulting the real `ssh -G alias` output
// (see resolveSSHConfig) for HostName/Port/User/IdentityFile overrides on
// the URI's host, the same way a plain `ssh alias` would. This matters
// beyond convenience: if the user's ~/.ssh/config redirects alias to a
// different HostName (a common pattern -- a short/internal name in the
// Host block, a real IP or FQDN as HostName), known_hosts records its entry
// under that resolved HostName, not the alias. Without this, vmsync would
// dial and check known_hosts under the literal alias from the URI, never
// matching the real entry -- observed directly as a spurious "knownhosts:
// key is unknown" against a host `ssh alias` itself connects to and
// verifies without complaint. Values already given explicitly (user,
// keyPath, port -- i.e. vmsync's own -ssh-user/-ssh-key/-ssh-port flags)
// always take precedence over whatever ssh_config says, mirroring how an
// explicit ssh command-line flag beats its own config file.
func ConfigFromLibvirtURI(libvirtURI, user, keyPath, password, knownHostsPath string, port int, insecure bool, timeout time.Duration) (Config, error) {
	u, err := url.Parse(libvirtURI)
	if err != nil {
		return Config{}, fmt.Errorf("parse libvirt uri %s: %w", libvirtURI, err)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("libvirt uri has no host: %s", libvirtURI)
	}
	alias := u.Hostname()
	sshConfig := resolveSSHConfig(alias)

	uriUser := ""
	if u.User != nil {
		uriUser = u.User.Username()
	}

	address, resolvedUser, resolvedKeyPath, resolvedPort, resolvedTimeout := resolveSSHConnectionParams(alias, uriUser, user, keyPath, port, timeout, sshConfig)

	trace.Debug("resolved ssh_config for host", "alias", alias, "address", address, "user", resolvedUser, "port", resolvedPort, "key", resolvedKeyPath)

	return Config{
		Address:               address,
		Port:                  resolvedPort,
		User:                  resolvedUser,
		PrivateKeyPath:        resolvedKeyPath,
		Password:              password,
		InsecureIgnoreHostKey: insecure,
		KnownHostsPath:        knownHostsPath,
		Timeout:               resolvedTimeout,
	}, nil
}

// resolveSSHConnectionParams decides the final address/user/keyPath/port/
// timeout ConfigFromLibvirtURI uses -- the exact precedence that governs
// where every SSH-executed command actually runs (and therefore, for
// example, what host -reinit's own disk-deletion command targets). Inputs:
// uriHost/uriUser come from the libvirt URI itself; user/keyPath/port/
// timeout are vmsync's own explicit CLI-flag overrides (-ssh-user/-ssh-key/
// -ssh-port/-ssh-timeout), which always win when set, regardless of source;
// sshConfig is `ssh -G <alias>`'s own resolved output (see resolveSSHConfig)
// for whatever an explicit flag didn't already decide.
//
// Split out from ConfigFromLibvirtURI specifically so this precedence is
// directly testable with a synthetic sshConfig map, without needing a real
// `ssh` binary or a real ~/.ssh/config the way resolveSSHConfig itself does.
func resolveSSHConnectionParams(uriHost, uriUser, user, keyPath string, port int, timeout time.Duration, sshConfig map[string]string) (address, resolvedUser, resolvedKeyPath string, resolvedPort int, resolvedTimeout time.Duration) {
	address = uriHost
	if hostname := sshConfig["hostname"]; hostname != "" {
		address = hostname
	}

	resolvedUser = user
	if resolvedUser == "" {
		resolvedUser = uriUser
	}
	if resolvedUser == "" {
		resolvedUser = sshConfig["user"]
	}
	if resolvedUser == "" {
		resolvedUser = "root"
	}

	resolvedPort = port
	if resolvedPort <= 0 {
		if portStr := sshConfig["port"]; portStr != "" {
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
				resolvedPort = p
			}
		}
	}
	if resolvedPort <= 0 {
		resolvedPort = 22
	}

	resolvedKeyPath = keyPath
	if resolvedKeyPath == "" {
		if idFile := sshConfig["identityfile"]; idFile != "" {
			resolvedKeyPath = expandHome(idFile)
		}
	}

	resolvedTimeout = timeout
	if resolvedTimeout <= 0 {
		resolvedTimeout = 10 * time.Second
	}

	return address, resolvedUser, resolvedKeyPath, resolvedPort, resolvedTimeout
}

// resolveSSHConfig shells out to the system's own `ssh -G alias` to get the
// fully-resolved ssh_config for alias, keyed by lowercased directive name
// (e.g. "hostname", "port", "user", "identityfile"). -G asks ssh to print
// its configuration after evaluating every Host/Match block and Include
// and exit -- no connection is made. This deliberately avoids
// reimplementing ssh_config parsing in Go at all: it's a real, actively
// evolving format (Match now has criteria like "canonical", "final",
// "exec", ...), and every third-party parser checked while debugging this
// (including the Go library tried here first) turned out to only support a
// subset of it, silently breaking every lookup -- not just the specific
// directive it choked on -- the moment a config used something outside
// that subset. Shelling out to the real `ssh` binary sidesteps that
// entirely: whatever this host's ssh already understands, this does too.
// Returns an empty map if ssh isn't installed, alias has no effective
// config, or -G fails for any reason -- every caller already has its own
// fallback for a key not being present.
func resolveSSHConfig(alias string) map[string]string {
	out, err := exec.Command("ssh", "-G", alias).Output()
	if err != nil {
		trace.Debug("ssh -G lookup failed, using literal values only", "alias", alias, "error", err)
		return nil
	}
	result := make(map[string]string, 8)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		key, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		key = strings.ToLower(key)
		// First occurrence wins -- matters for identityfile, where ssh -G
		// lists the user's own explicit IdentityFile directive(s) before
		// its built-in default candidates (id_rsa, id_ed25519, ...), in
		// the same order ssh itself would try them.
		if _, exists := result[key]; !exists {
			result[key] = value
		}
	}
	return result
}

// expandHome resolves a leading "~" in an ssh_config IdentityFile value
// (e.g. "~/.ssh/id_ed25519") -- Go's file APIs, unlike a shell, never do
// this expansion themselves.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func Dial(cfg Config) (*Client, error) {
	hostKeyCallback, hostKeyAlgorithms, err := buildHostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}

	authMethods, agentConn, err := buildAuthMethods(cfg)
	if err != nil {
		return nil, err
	}
	// Only needed for the handshake below (ssh.NewClientConn invokes the
	// agent callback synchronously during auth, not afterward) -- close it
	// once Dial returns either way, rather than leaking it for the life of
	// the process the way this connection previously was.
	if agentConn != nil {
		defer agentConn.Close()
	}

	sshCfg := &ssh.ClientConfig{
		User: cfg.User,
		Auth: authMethods,
		// Empty (not nil) for an as-yet-unknown host, or when
		// InsecureIgnoreHostKey is set -- the ssh library falls back to its
		// own default algorithm order in that case, same as before this
		// fix; see buildHostKeyCallback's own comment for why this is set
		// at all.
		HostKeyAlgorithms: hostKeyAlgorithms,
		HostKeyCallback:   hostKeyCallback,
		Timeout:           cfg.Timeout,
	}

	address := net.JoinHostPort(cfg.Address, fmt.Sprintf("%d", cfg.Port))

	// ssh.Dial's own Timeout field only bounds the initial TCP connect --
	// the SSH handshake and authentication that follow it have no timeout
	// of their own, a well-documented golang.org/x/crypto/ssh limitation
	// (see golang/go issues #50046 and #51926: "Dial hangs in kexLoop
	// indefinitely - ignoring ClientConfig.Timeout"). Against a host that
	// accepts the TCP connection but never completes (or never finishes)
	// the SSH protocol exchange -- observed directly as vmsync instances
	// stuck forever against an unreachable remote -- ssh.Dial itself can
	// hang past cfg.Timeout with no way to bound it from outside, since it
	// takes no context either. Dial the TCP connection ourselves instead
	// and put a deadline on it that covers the handshake, via the same two
	// calls (NewClientConn + NewClient) ssh.Dial is implemented as -- then
	// clear the deadline once the connection is up so it doesn't limit the
	// connection's actual, ongoing lifetime afterward.
	conn, err := net.DialTimeout("tcp", address, cfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", address, err)
	}
	if err := conn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh dial %s: set handshake deadline: %w", address, err)
	}
	// A PublicKeysCallback auth method (SSH_AUTH_SOCK) is invoked
	// synchronously during the handshake below, but over agentConn -- a
	// wholly separate file descriptor to a local ssh-agent that conn's own
	// deadline above has no effect on. A stale or unresponsive agent (e.g.
	// a forwarded socket left over from an ended session) would otherwise
	// block the handshake indefinitely, defeating the point of bounding it
	// at all. Give it the same deadline window as the handshake itself.
	if agentConn != nil {
		if err := agentConn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ssh dial %s: set ssh-agent deadline: %w", address, err)
		}
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, sshCfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh dial %s: %w", address, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, fmt.Errorf("ssh dial %s: clear handshake deadline: %w", address, err)
	}

	c := ssh.NewClient(sshConn, chans, reqs)
	out := &Client{cfg: cfg, client: c}
	// Started with the connection, so every caller gets a bounded detection
	// window without having to ask for one.
	out.startKeepalive()
	return out, nil
}

func (c *Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	// The prober first, so it cannot be probing a connection this call is
	// closing and report that as a peer that stopped answering.
	c.stopKeepalive()
	return c.client.Close()
}

// DialTCP opens a direct-tcpip channel to addr (host:port) through this
// client's SSH connection, so the remote endpoint only has to be reachable
// from the remote host itself, not from the machine running vmsync.
func (c *Client) DialTCP(addr string) (net.Conn, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("ssh client is not connected")
	}
	return c.client.Dial("tcp", addr)
}

// RunWithInput runs command with stdin fed from input, returning stdout and
// stderr SEPARATELY.
//
// Both differences from Run matter, and both exist for the digest exchange
// (see pkg/blockdigest). That exchange sends a request on stdin, which Run
// cannot do at all; and its reply is parsed, which Run's CombinedOutput
// would corrupt the moment the remote command wrote a single warning to
// stderr -- the diagnostic would be interleaved into the data stream and
// read as a malformed digest line. Keeping the two streams apart means a
// remote warning stays diagnosable instead of becoming a parse error, and a
// parse error genuinely means the data was wrong.
//
// stderr is returned rather than logged here so the caller can fold it into
// its own error message, which is where the operator will actually look.
// lineWriter calls onLine once per complete line written to it, and is how a
// remote command's stderr reaches the log WHILE it runs rather than after.
//
// A buffered-until-the-end stderr is fine for a message explaining a failure,
// which is all it was ever used for. It is useless for anything the remote
// side says about work in progress: the pass that most needs to be audible --
// the target's digest of a terabyte -- says it over tens of minutes, and the
// one run that mattered died before the session closed, taking every word of
// it with the buffer.
//
// Partial trailing output is deliberately dropped rather than logged: a
// progress line is worth nothing once it is the only thing left of a dead
// session, and the full text is still in the buffer the caller gets back.
type lineWriter struct {
	onLine func(string)
	buf    []byte
	// discard is set while an over-long line is being thrown away, and
	// cleared by the newline that ends it.
	//
	// Not an optimisation -- a correctness requirement. Dropping the
	// oversized buffer and carrying on would leave whatever arrived after it
	// in the same line, so the next real line would be logged with a
	// kilobyte of someone else's garbage glued to its front. Resyncing on
	// the newline is what makes the line after an overflow intact.
	discard bool
}

// maxLineBytes bounds one line. The far side is a process on another host, so
// a bug or a binary stream over there must not be able to grow this buffer
// until vmsync runs out of memory.
const maxLineBytes = 64 * 1024

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !w.discard {
				w.buf = append(w.buf, p...)
				if len(w.buf) > maxLineBytes {
					w.buf = w.buf[:0]
					w.discard = true
				}
			}
			break
		}
		if w.discard {
			w.discard = false
		} else {
			w.buf = append(w.buf, p[:i]...)
			line := strings.TrimRight(string(w.buf), "\r")
			w.buf = w.buf[:0]
			if line != "" {
				w.onLine(line)
			}
		}
		p = p[i+1:]
	}
	return n, nil
}

func (c *Client) RunWithInput(ctx context.Context, command string, input []byte) (stdout string, stderr string, err error) {
	return c.RunWithInputProgress(ctx, command, input, nil)
}

// RunWithInputProgress is RunWithInput plus onStderrLine, called once per
// line the command writes to stderr, as it is written. stderr is still
// returned in full.
//
// onStderrLine is called from the goroutine copying the session's stderr, so
// it must be safe to call concurrently with the caller's own work and must
// not block for long.
func (c *Client) RunWithInputProgress(ctx context.Context, command string, input []byte, onStderrLine func(string)) (stdout string, stderr string, err error) {
	if c == nil || c.client == nil {
		return "", "", errors.New("ssh client is not connected")
	}

	type sessionResult struct {
		session *ssh.Session
		err     error
	}
	sessCh := make(chan sessionResult, 1)
	go func() {
		session, err := c.client.NewSession()
		sessCh <- sessionResult{session: session, err: err}
	}()

	var session *ssh.Session
	select {
	case <-ctx.Done():
		go func() {
			if r := <-sessCh; r.session != nil {
				r.session.Close()
			}
		}()
		return "", "", fmt.Errorf("open ssh session: %w", ctx.Err())
	case r := <-sessCh:
		if r.err != nil {
			return "", "", fmt.Errorf("open ssh session: %w", r.err)
		}
		session = r.session
	}
	defer session.Close()

	var outBuf, errBuf bytes.Buffer
	session.Stdout = &outBuf
	if onStderrLine != nil {
		session.Stderr = io.MultiWriter(&errBuf, &lineWriter{onLine: onStderrLine})
	} else {
		session.Stderr = &errBuf
	}
	// A plain bytes.Reader rather than an explicit StdinPipe: x/crypto/ssh
	// copies from session.Stdin and closes the remote's stdin when it is
	// exhausted, which is exactly the EOF a one-shot command needs to stop
	// reading. Driving a StdinPipe by hand would mean owning that close, and
	// forgetting it deadlocks both ends.
	session.Stdin = bytes.NewReader(input)

	ch := make(chan error, 1)
	go func() { ch <- session.Run(command) }()

	select {
	case <-ctx.Done():
		_ = session.Close()
		return "", "", ctx.Err()
	case runErr := <-ch:
		stdout = outBuf.String()
		stderr = strings.TrimSpace(errBuf.String())
		if runErr != nil {
			return stdout, stderr, fmt.Errorf("ssh run command %q: %w", command, runErr)
		}
		return stdout, stderr, nil
	}
}

func (c *Client) Run(ctx context.Context, command string) (string, error) {
	if c == nil || c.client == nil {
		return "", errors.New("ssh client is not connected")
	}

	// NewSession blocks on the underlying SSH transport with no timeout of
	// its own; if that transport is wedged (e.g. the connection is alive at
	// the TCP level but no longer servicing channel requests), this would
	// otherwise hang forever regardless of ctx. Race it against ctx instead.
	type sessionResult struct {
		session *ssh.Session
		err     error
	}
	sessCh := make(chan sessionResult, 1)
	go func() {
		session, err := c.client.NewSession()
		sessCh <- sessionResult{session: session, err: err}
	}()

	var session *ssh.Session
	select {
	case <-ctx.Done():
		// Best-effort: close the session if NewSession eventually completes,
		// so it isn't leaked -- but don't make the caller wait for it.
		go func() {
			if r := <-sessCh; r.session != nil {
				r.session.Close()
			}
		}()
		return "", fmt.Errorf("open ssh session: %w", ctx.Err())
	case r := <-sessCh:
		if r.err != nil {
			return "", fmt.Errorf("open ssh session: %w", r.err)
		}
		session = r.session
	}
	defer session.Close()

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, runErr := session.CombinedOutput(command)
		ch <- result{out: out, err: runErr}
	}()

	select {
	case <-ctx.Done():
		_ = session.Close()
		return "", ctx.Err()
	case r := <-ch:
		outText := strings.TrimSpace(string(r.out))
		if r.err != nil {
			return outText, fmt.Errorf("ssh run command %q: %w", command, r.err)
		}
		return outText, nil
	}
}

// buildAuthMethods also returns the raw connection to a local ssh-agent
// (SSH_AUTH_SOCK), when one of the returned methods uses it, so the caller
// can bound how long the handshake is allowed to wait on it -- see the
// comment where Dial sets its deadline for why that matters. nil when no
// agent method was added.
func buildAuthMethods(cfg Config) ([]ssh.AuthMethod, net.Conn, error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn
	if cfg.PrivateKeyPath != "" {
		signer, err := signerFromPath(cfg.PrivateKeyPath)
		if err != nil {
			return nil, nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			agentConn = conn
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}

	if len(methods) == 0 {
		return nil, nil, errors.New("no ssh auth method available: provide --ssh-key, --ssh-password, or SSH_AUTH_SOCK")
	}
	return methods, agentConn, nil
}

func signerFromPath(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ssh key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key %s: %w", path, err)
	}
	return signer, nil
}

// buildHostKeyCallback returns both the host key verification callback and
// the host key algorithms to request during key exchange, sourced from the
// same known_hosts data. Both are needed together because of a
// well-documented gap in the plain golang.org/x/crypto/ssh/knownhosts
// package (see golang/go#49631): left to its own defaults, the Go SSH
// client's preferred host key algorithm order can differ from OpenSSH's, so
// a server offering multiple host key types (RSA, ECDSA, ed25519 -- common)
// may end up presenting a DIFFERENT, equally valid type than the one
// actually recorded in known_hosts. The result is a spurious "knownhosts:
// key is unknown" even though the host genuinely is trusted -- observed
// directly against a host that a plain `ssh` connected to without any
// complaint, using the exact same known_hosts file. github.com/skeema/knownhosts
// wraps the same file/format but additionally exposes the algorithm(s)
// actually recorded for a given host, letting the client request exactly
// those up front instead of leaving the choice to chance.
func buildHostKeyCallback(cfg Config) (ssh.HostKeyCallback, []string, error) {
	if cfg.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil, nil
	}
	knownHostsPath := cfg.KnownHostsPath
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("get user home for known_hosts: %w", err)
		}
		knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	db, err := knownhosts.NewDB(knownHostsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load known_hosts %s: %w", knownHostsPath, err)
	}
	hostWithPort := net.JoinHostPort(cfg.Address, fmt.Sprintf("%d", cfg.Port))
	return db.HostKeyCallback(), db.HostKeyAlgorithms(hostWithPort), nil
}
