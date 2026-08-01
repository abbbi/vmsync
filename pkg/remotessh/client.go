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
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
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

func ConfigFromLibvirtURI(libvirtURI, user, keyPath, password, knownHostsPath string, port int, insecure bool, timeout time.Duration) (Config, error) {
	u, err := url.Parse(libvirtURI)
	if err != nil {
		return Config{}, fmt.Errorf("parse libvirt uri %s: %w", libvirtURI, err)
	}
	if u.Host == "" {
		return Config{}, fmt.Errorf("libvirt uri has no host: %s", libvirtURI)
	}

	resolvedUser := user
	if resolvedUser == "" && u.User != nil {
		resolvedUser = u.User.Username()
	}
	if resolvedUser == "" {
		resolvedUser = "root"
	}
	if port <= 0 {
		port = 22
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return Config{
		Address:               u.Hostname(),
		Port:                  port,
		User:                  resolvedUser,
		PrivateKeyPath:        keyPath,
		Password:              password,
		InsecureIgnoreHostKey: insecure,
		KnownHostsPath:        knownHostsPath,
		Timeout:               timeout,
	}, nil
}

func Dial(cfg Config) (*Client, error) {
	hostKeyCallback, err := buildHostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}

	authMethods, err := buildAuthMethods(cfg)
	if err != nil {
		return nil, err
	}

	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         cfg.Timeout,
	}

	address := net.JoinHostPort(cfg.Address, fmt.Sprintf("%d", cfg.Port))
	c, err := ssh.Dial("tcp", address, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", address, err)
	}
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

func buildHostKeyCallback(cfg Config) (ssh.HostKeyCallback, error) {
	if cfg.InsecureIgnoreHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	knownHostsPath := cfg.KnownHostsPath
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get user home for known_hosts: %w", err)
		}
		knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", knownHostsPath, err)
	}
	return cb, nil
}
