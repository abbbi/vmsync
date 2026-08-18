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

package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The control-plane UI is a separate program in its own repository, so the
// HTTP surface below is a contract between two codebases rather than an
// internal detail. These tests drive a stub UI and are the executable
// specification that one has to satisfy: the paths, the auth header, the
// status codes and the ETag/long-poll behaviour are all pinned here.

// stubUI runs an in-process TLS server standing in for the UI, and returns a
// client already pointed at it. handler sees every request.
func stubUI(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return &Client{
		Base: srv.URL,
		HTTP: srv.Client(),
		Creds: Credentials{
			AgentID: "agent-7",
			Token:   "long-lived-token",
			UIBase:  srv.URL,
		},
	}, srv
}

func TestEnrolExchangesTheTokenForACredential(t *testing.T) {
	var gotBody enrolRequest
	c, srv := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != pathEnrol {
			t.Errorf("enrolment hit %s %s, want POST %s", r.Method, r.URL.Path, pathEnrol)
		}
		// Enrolment is the one call with no bearer token -- the enrolment
		// token in the body is the credential being spent.
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("enrolment sent Authorization: %q, want none", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode enrolment body: %v", err)
		}
		json.NewEncoder(w).Encode(enrolResponse{AgentID: "agent-7", Token: "long-lived-token"})
	})

	creds, err := c.Enrol(context.Background(), "hyper01p", "one-time-token", "0.30")
	if err != nil {
		t.Fatalf("Enrol() error = %v", err)
	}
	if creds.AgentID != "agent-7" || creds.Token != "long-lived-token" {
		t.Errorf("Enrol() = %+v, want the agent_id and token the UI issued", creds)
	}
	// Recorded so that repointing the agent at a different UI is detected as
	// needing re-enrolment rather than silently presenting an unknown token.
	if creds.UIBase != srv.URL {
		t.Errorf("credentials recorded UIBase=%q, want %q", creds.UIBase, srv.URL)
	}
	if gotBody.Hostname != "hyper01p" || gotBody.EnrolmentToken != "one-time-token" {
		t.Errorf("enrolment request = %+v, want the hostname and token passed in", gotBody)
	}
	if gotBody.AgentVersion != "0.30" {
		t.Errorf("enrolment did not report its version: %+v", gotBody)
	}
}

func TestEnrolRejectsAnIncompleteResponse(t *testing.T) {
	// A UI that answers 200 with a missing token would otherwise leave the
	// agent storing an empty credential and failing every later call with a
	// confusing 401 instead of the real problem.
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(enrolResponse{AgentID: "agent-7"})
	})
	if _, err := c.Enrol(context.Background(), "hyper01p", "tok", "0.30"); err == nil {
		t.Fatal("Enrol() accepted a response with no token")
	}
}

func TestEnrolSpentTokenIsReportedAsRevoked(t *testing.T) {
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "enrolment token already used", http.StatusUnauthorized)
	})
	_, err := c.Enrol(context.Background(), "hyper01p", "already-spent", "0.30")
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("Enrol() with a spent token = %v, want ErrRevoked", err)
	}
}

func TestSendReportPostsToTheAgentsOwnPath(t *testing.T) {
	var got Report
	var authHeader, path string
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authHeader = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	})

	report := Report{
		ReportedAtUnix: 1_800_000_000,
		Hostname:       "hyper01p",
		LibvirtURI:     "qemu:///system",
		Domains: []ReportDomain{{
			Name:          "web01",
			ReplicaSource: "hyper01p:web01",
			Status:        "ok",
			AgeSeconds:    42,
		}},
	}
	if err := c.SendReport(context.Background(), report); err != nil {
		t.Fatalf("SendReport() error = %v", err)
	}

	if want := pathAgents + "agent-7/report"; path != want {
		t.Errorf("report posted to %q, want %q", path, want)
	}
	if authHeader != "Bearer long-lived-token" {
		t.Errorf("Authorization = %q, want the bearer credential", authHeader)
	}
	if len(got.Domains) != 1 || got.Domains[0].Name != "web01" {
		t.Errorf("UI received %+v, want the single reported domain", got.Domains)
	}
	if got.Domains[0].Status != "ok" {
		t.Errorf("status arrived as %q, want the string form -- an integer would tie the wire format to iota ordering", got.Domains[0].Status)
	}
}

func TestRevokedCredentialIsDistinguishedFromEveryOtherFailure(t *testing.T) {
	// Worth its own error: every other failure is worth retrying, this one
	// never succeeds until an operator issues a fresh enrolment token. An
	// agent that retried it forever would hide a real revocation behind
	// ordinary-looking connection noise.
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", code)
		})
		if err := c.SendReport(context.Background(), Report{}); !errors.Is(err, ErrRevoked) {
			t.Errorf("SendReport() against %d = %v, want ErrRevoked", code, err)
		}
		if _, _, err := c.PollConfig(context.Background(), "", time.Second); !errors.Is(err, ErrRevoked) {
			t.Errorf("PollConfig() against %d = %v, want ErrRevoked", code, err)
		}
	}
}

func TestPollConfigSendsTheWaitAndCachedETag(t *testing.T) {
	var gotWait, gotINM string
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		gotWait = r.URL.Query().Get("wait")
		gotINM = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"v2"`)
		json.NewEncoder(w).Encode(UIConfig{ReportIntervalSeconds: 120, PollWaitSeconds: 45})
	})

	cfg, etag, err := c.PollConfig(context.Background(), `"v1"`, 30*time.Second)
	if err != nil {
		t.Fatalf("PollConfig() error = %v", err)
	}
	if gotWait != "30" {
		t.Errorf("wait query = %q, want \"30\" -- the UI needs it to know how long to hold the request", gotWait)
	}
	if gotINM != `"v1"` {
		t.Errorf("If-None-Match = %q, want the cached etag so the UI can answer 304", gotINM)
	}
	if etag != `"v2"` {
		t.Errorf("returned etag = %q, want the one the UI sent", etag)
	}
	if cfg.ReportIntervalSeconds != 120 || cfg.PollWaitSeconds != 45 {
		t.Errorf("config = %+v, want the values the UI sent", cfg)
	}
}

func TestPollConfigUnchangedIsNotAFailure(t *testing.T) {
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	_, _, err := c.PollConfig(context.Background(), `"v1"`, time.Second)
	if !errors.Is(err, ErrUnchanged) {
		t.Fatalf("PollConfig() against 304 = %v, want ErrUnchanged", err)
	}
}

func TestPollConfigNormalizesNonsenseFromTheUI(t *testing.T) {
	// The UI is a separately-versioned program, so its answers are input to
	// validate rather than trusted state. A zero interval would turn the
	// agent's loop into a busy loop hammering the UI.
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UIConfig{ReportIntervalSeconds: 0, PollWaitSeconds: -5})
	})
	cfg, _, err := c.PollConfig(context.Background(), "", time.Second)
	if err != nil {
		t.Fatalf("PollConfig() error = %v", err)
	}
	d := DefaultUIConfig()
	if cfg.ReportIntervalSeconds != d.ReportIntervalSeconds || cfg.PollWaitSeconds != d.PollWaitSeconds {
		t.Errorf("config = %+v, want the defaults substituted for the nonsense values", cfg)
	}
}

func TestServerErrorsCarryTheirDetailBack(t *testing.T) {
	// This surfaces in a systemd journal as the agent's only clue, so the
	// UI's own explanation has to survive the trip.
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "database is locked", http.StatusInternalServerError)
	})
	err := c.SendReport(context.Background(), Report{})
	if err == nil {
		t.Fatal("SendReport() against a 500 returned no error")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("error %q does not carry the UI's own explanation", err.Error())
	}
	if errors.Is(err, ErrRevoked) {
		t.Error("a 500 was misreported as a revoked credential")
	}
}

func TestNewClientRefusesPlaintextAndTrustsAGivenCA(t *testing.T) {
	t.Run("plaintext http is refused outright", func(t *testing.T) {
		// The agent's credential and the estate's replication topology
		// travel over this connection.
		if _, err := NewClient("http://ui.example.org", "", time.Second); err == nil {
			t.Fatal("NewClient accepted an http:// UI address")
		}
	})

	t.Run("a private CA bundle is honoured", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(enrolResponse{AgentID: "a1", Token: "t1"})
		}))
		defer srv.Close()

		// The stub server's own certificate, written out the way an operator
		// would supply a self-signed UI's CA.
		caPath := filepath.Join(t.TempDir(), "ui-ca.pem")
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		if err := os.WriteFile(caPath, pemBytes, 0o644); err != nil {
			t.Fatalf("write CA bundle: %v", err)
		}

		c, err := NewClient(srv.URL, caPath, 5*time.Second)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := c.Enrol(context.Background(), "hyper01p", "tok", "0.30"); err != nil {
			t.Fatalf("Enrol() through the supplied CA bundle failed: %v", err)
		}
	})

	t.Run("without the CA bundle the same server is rejected", func(t *testing.T) {
		// The other half of the assertion above: verification is genuinely
		// happening, rather than the bundle being decorative.
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()

		c, err := NewClient(srv.URL, "", 5*time.Second)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := c.Enrol(context.Background(), "hyper01p", "tok", "0.30"); err == nil {
			t.Fatal("Enrol() trusted a self-signed certificate with no CA bundle configured")
		}
	})

	t.Run("an unusable CA bundle is refused at construction", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "not-a-cert.pem")
		os.WriteFile(bad, []byte("this is not a certificate"), 0o644)
		if _, err := NewClient("https://ui.example.org", bad, time.Second); err == nil {
			t.Fatal("NewClient accepted a CA bundle containing no certificate")
		}
	})
}
