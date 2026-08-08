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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLoopbackSelfAddress(t *testing.T) {
	cases := []struct {
		name string
		port int
		want string
	}{
		{"typical port", 2222, "127.0.0.1:2222"},
		{"default ssh port", 22, "127.0.0.1:22"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{cfg: Config{Port: tc.port}}
			if got := c.LoopbackSelfAddress(); got != tc.want {
				t.Errorf("LoopbackSelfAddress() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available in this environment: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde slash path", "~/.ssh/id_ed25519", filepath.Join(home, ".ssh/id_ed25519")},
		{"absolute path unchanged", "/etc/ssh/known_hosts", "/etc/ssh/known_hosts"},
		{"relative path unchanged", "id_rsa", "id_rsa"},
		{"tilde for another user is not expanded", "~otheruser/id_rsa", "~otheruser/id_rsa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandHome(tc.in); got != tc.want {
				t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// generateTestRSAKey produces a real, disposable RSA key pair and its
// PEM-encoded (PKCS1, "RSA PRIVATE KEY") private key bytes -- the classic
// format ssh.ParsePrivateKey has always supported, used purely as a test
// fixture, never anything real/deployed.
func generateTestRSAKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test rsa key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return key, pem.EncodeToMemory(block)
}

func TestSignerFromPath(t *testing.T) {
	t.Run("valid key", func(t *testing.T) {
		_, pemBytes := generateTestRSAKey(t)
		keyPath := filepath.Join(t.TempDir(), "id_rsa")
		if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
			t.Fatalf("write test key: %v", err)
		}
		signer, err := signerFromPath(keyPath)
		if err != nil {
			t.Fatalf("signerFromPath() error = %v, want nil", err)
		}
		if signer == nil {
			t.Fatal("signerFromPath() returned nil signer with nil error")
		}
	})

	t.Run("nonexistent file", func(t *testing.T) {
		_, err := signerFromPath(filepath.Join(t.TempDir(), "does-not-exist"))
		if err == nil {
			t.Fatal("expected an error for a nonexistent key file")
		}
	})

	t.Run("garbage content", func(t *testing.T) {
		keyPath := filepath.Join(t.TempDir(), "garbage")
		if err := os.WriteFile(keyPath, []byte("this is not a key"), 0600); err != nil {
			t.Fatalf("write garbage file: %v", err)
		}
		if _, err := signerFromPath(keyPath); err == nil {
			t.Fatal("expected an error for a file that isn't a valid ssh key")
		}
	})
}

func TestBuildHostKeyCallback(t *testing.T) {
	t.Run("insecure ignore host key needs no file access", func(t *testing.T) {
		cb, algos, err := buildHostKeyCallback(Config{InsecureIgnoreHostKey: true})
		if err != nil {
			t.Fatalf("buildHostKeyCallback() error = %v, want nil", err)
		}
		if cb == nil {
			t.Fatal("expected a non-nil HostKeyCallback")
		}
		if algos != nil {
			t.Errorf("expected nil algorithms for the insecure branch, got %v", algos)
		}
	})

	t.Run("real known_hosts file", func(t *testing.T) {
		key, _ := generateTestRSAKey(t)
		pub, err := ssh.NewPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("derive public key: %v", err)
		}
		line := "testhost " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))

		knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.WriteFile(knownHostsPath, []byte(line+"\n"), 0600); err != nil {
			t.Fatalf("write known_hosts fixture: %v", err)
		}

		cb, _, err := buildHostKeyCallback(Config{
			KnownHostsPath: knownHostsPath,
			Address:        "testhost",
			Port:           22,
		})
		if err != nil {
			t.Fatalf("buildHostKeyCallback() error = %v, want nil", err)
		}
		if cb == nil {
			t.Fatal("expected a non-nil HostKeyCallback")
		}
	})

	t.Run("missing known_hosts file is an error", func(t *testing.T) {
		_, _, err := buildHostKeyCallback(Config{
			KnownHostsPath: filepath.Join(t.TempDir(), "does-not-exist"),
			Address:        "testhost",
			Port:           22,
		})
		if err == nil {
			t.Fatal("expected an error when the known_hosts file doesn't exist")
		}
	})
}

func TestBuildAuthMethodsNoMethodsAvailable(t *testing.T) {
	// Empty string has the same effect as "unset" on the one check this
	// function makes (os.Getenv(...) != ""), and t.Setenv restores the
	// original value automatically once the test finishes.
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, _, err := buildAuthMethods(Config{}); err == nil {
		t.Fatal("expected an error when no key, password, or SSH agent is configured")
	}
}

// TestBuildAuthMethodsReturnsAgentConn pins down the return value Dial
// depends on to bound the ssh-agent handshake with its own deadline (see
// Dial's comment on agentConn) -- it must be the same raw connection
// PublicKeysCallback was built from, not nil, whenever SSH_AUTH_SOCK
// resolves to a real socket. The fake listener here doesn't need to speak
// the real ssh-agent wire protocol; this only exercises the plumbing, not
// an actual agent round trip (real agent conversations are exercised, if at
// all, only through a live Dial -- out of scope here, same as Dial itself).
func TestBuildAuthMethodsReturnsAgentConn(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on fake agent socket: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	t.Setenv("SSH_AUTH_SOCK", sockPath)
	methods, agentConn, err := buildAuthMethods(Config{})
	if err != nil {
		t.Fatalf("buildAuthMethods() error = %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("expected exactly one auth method from the agent socket, got %d", len(methods))
	}
	if agentConn == nil {
		t.Fatal("expected the raw agent connection back so Dial can bound its deadline, got nil")
	}
	agentConn.Close()
}

func TestBuildAuthMethodsNoAgentConnWithoutSSHAuthSock(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, agentConn, err := buildAuthMethods(Config{Password: "x"})
	if err != nil {
		t.Fatalf("buildAuthMethods() error = %v", err)
	}
	if agentConn != nil {
		t.Fatal("expected a nil agent connection when SSH_AUTH_SOCK is unset")
	}
}
