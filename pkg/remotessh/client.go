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
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kevinburke/ssh_config"
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
}

// LoopbackSelfAddress returns 127.0.0.1:<ssh-port> for this client's remote
// host -- a destination sshd itself is always listening on, usable as a
// self-test direct-tcpip dial target without depending on hostname
// resolution working the same way from the remote side as it did locally.
func (c *Client) LoopbackSelfAddress() string {
	return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", c.cfg.Port))
}

// ConfigFromLibvirtURI builds a Config from a libvirt connection URI
// (e.g. qemu+ssh://alias/system), consulting ~/.ssh/config (and
// /etc/ssh/ssh_config, via ssh_config.Get own standard lookup) for
// HostName/Port/User/IdentityFile overrides on the URI's host, the same way
// a plain `ssh alias` would. This matters beyond convenience: if the
// user's ~/.ssh/config redirects alias to a different HostName (a common
// pattern -- a short/internal name in the Host block, a real IP or FQDN as
// HostName), known_hosts records its entry under that resolved HostName,
// not the alias. Without this, vmsync would dial and check known_hosts
// under the literal alias from the URI, never matching the real entry --
// observed directly as a spurious "knownhosts: key is unknown" against a
// host `ssh alias` itself connects to and verifies without complaint.
// Values already given explicitly (user, keyPath, port -- i.e. vmsync's own
// -ssh-user/-ssh-key/-ssh-port flags) always take precedence over whatever
// ~/.ssh/config says, mirroring how an explicit ssh command-line flag beats
// its own config file.
func ConfigFromLibvirtURI(libvirtURI, user, keyPath, password, knownHostsPath string, port int, insecure bool, timeout time.Duration) (Config, error) {
	u, err := url.Parse(libvirtURI)
	if err != nil {
		return Config{}, fmt.Errorf("parse libvirt uri %s: %w", libvirtURI, err)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("libvirt uri has no host: %s", libvirtURI)
	}
	alias := u.Hostname()

	address := alias
	if hostname := ssh_config.Get(alias, "HostName"); hostname != "" {
		address = hostname
	}
	// Temporary diagnostic: ssh_config.Get silently returns "" on any
	// lookup problem (parse error, wrong $HOME, file not found), so a
	// no-op resolution here is otherwise indistinguishable from "no
	// override configured for this host". home/homeErr uses the same
	// resolution as this package's own known_hosts default path, for a
	// direct comparison against what ssh_config's own (separate) home
	// directory lookup found.
	home, homeErr := os.UserHomeDir()
	trace.Debug("ssh_config host resolution", "alias", alias, "resolved_address", address, "home", home, "home_err", homeErr)

	resolvedUser := user
	if resolvedUser == "" && u.User != nil {
		resolvedUser = u.User.Username()
	}
	if resolvedUser == "" {
		resolvedUser = ssh_config.Get(alias, "User")
	}
	if resolvedUser == "" {
		resolvedUser = "root"
	}

	if port <= 0 {
		if portStr := ssh_config.Get(alias, "Port"); portStr != "" {
			if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
				port = p
			}
		}
	}
	if port <= 0 {
		port = 22
	}

	resolvedKeyPath := keyPath
	if resolvedKeyPath == "" {
		if idFile := ssh_config.Get(alias, "IdentityFile"); idFile != "" {
			resolvedKeyPath = expandHome(idFile)
		}
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return Config{
		Address:               address,
		Port:                  port,
		User:                  resolvedUser,
		PrivateKeyPath:        resolvedKeyPath,
		Password:              password,
		InsecureIgnoreHostKey: insecure,
		KnownHostsPath:        knownHostsPath,
		Timeout:               timeout,
	}, nil
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

	authMethods, err := buildAuthMethods(cfg)
	if err != nil {
		return nil, err
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
	return &Client{cfg: cfg, client: c}, nil
}

func (c *Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
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

func buildAuthMethods(cfg Config) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if cfg.PrivateKeyPath != "" {
		signer, err := signerFromPath(cfg.PrivateKeyPath)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}

	if len(methods) == 0 {
		return nil, errors.New("no ssh auth method available: provide --ssh-key, --ssh-password, or SSH_AUTH_SOCK")
	}
	return methods, nil
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
