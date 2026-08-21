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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The agent-facing HTTP API, in one place. The UI is a separate program, so
// these paths and payloads are a contract between two codebases rather than
// an internal detail -- client_test.go exercises every one of them against a
// stub server and is the executable specification the UI must satisfy.
//
//	POST {base}/api/v1/agents/enrol
//	     -> {"agent_id","token"}                       enrolment token spent
//	POST {base}/api/v1/agents/{id}/report   (bearer)   inventory upload
//	GET  {base}/api/v1/agents/{id}/config   (bearer)   long-poll, ETag-aware
const (
	pathEnrol  = "/api/v1/agents/enrol"
	pathAgents = "/api/v1/agents/"
)

// ErrRevoked reports that the UI no longer accepts this agent's credential.
// Distinguished from any other failure because the response differs: every
// other error is worth retrying, this one never succeeds without an
// operator issuing a fresh enrolment token.
var ErrRevoked = errors.New("agent credential rejected by the UI (revoked, or the UI's state was reset)")

// ErrUnchanged reports that a config poll returned 304: the agent's cached
// configuration is still current. Not a failure.
var ErrUnchanged = errors.New("configuration unchanged")

// Client talks to the control-plane UI.
type Client struct {
	Base  string
	HTTP  *http.Client
	Creds Credentials
}

// NewClient builds a client that verifies the UI's certificate.
//
// caFile, when set, is used INSTEAD of the system pool, which is what makes
// a self-signed or private-CA UI workable without weakening anything.
// There is deliberately no way to skip verification: an agent holds a
// credential and reports the estate's replication topology, and an
// --insecure flag would be the first thing reached for on a certificate
// problem and the last thing anyone removed afterward.
func NewClient(base, caFile string, timeout time.Duration) (*Client, error) {
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("invalid UI address %q: %w", base, err)
	}
	if !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("UI address %q must be https:// -- the agent's credential and the estate's topology travel over this connection", base)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read UI CA bundle %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("UI CA bundle %s contains no usable certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &Client{
		Base: strings.TrimRight(base, "/"),
		HTTP: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

type enrolRequest struct {
	Hostname       string `json:"hostname"`
	EnrolmentToken string `json:"enrolment_token"`
	AgentVersion   string `json:"agent_version"`
}

type enrolResponse struct {
	AgentID string `json:"agent_id"`
	Token   string `json:"token"`
}

// Enrol exchanges a single-use enrolment token for a long-lived credential.
//
// The token is spent by this call: it is generated in the UI against a named
// host and cannot be replayed. That is what makes it safe for the token to
// travel by whatever means an operator has to hand -- if it leaks after
// enrolment, it is already worthless.
func (c *Client) Enrol(ctx context.Context, hostname, enrolmentToken, agentVersion string) (Credentials, error) {
	body, err := c.do(ctx, http.MethodPost, c.Base+pathEnrol, "", enrolRequest{
		Hostname:       hostname,
		EnrolmentToken: enrolmentToken,
		AgentVersion:   agentVersion,
	}, nil, nil)
	if err != nil {
		return Credentials{}, err
	}
	var resp enrolResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Credentials{}, fmt.Errorf("parse enrolment response: %w", err)
	}
	if resp.AgentID == "" || resp.Token == "" {
		return Credentials{}, fmt.Errorf("enrolment response is missing agent_id or token")
	}
	return Credentials{AgentID: resp.AgentID, Token: resp.Token, UIBase: c.Base}, nil
}

// Report is one inventory upload.
type Report struct {
	ReportedAtUnix int64          `json:"reported_at_unix"`
	AgentVersion   string         `json:"agent_version"`
	Hostname       string         `json:"hostname"`
	LibvirtURI     string         `json:"libvirt_uri"`
	Domains        []ReportDomain `json:"domains"`
	// ConfigAgeSeconds is how long since this agent last confirmed its
	// configuration with the UI. It lets the UI show that an agent is
	// running on stale instructions -- the expected state during a
	// partition, and something an operator needs to see rather than guess.
	ConfigAgeSeconds int64 `json:"config_age_seconds"`
	// Syncs are recent scheduled-run outcomes, so an operator can see what
	// happened without reading a journal on the host. Bounded by the agent.
	Syncs []SyncResult `json:"syncs,omitempty"`
	// OperationResults carries the outcome of every operation this agent
	// still holds a ledger record for, re-sent on EVERY report until the UI
	// stops publishing the operation.
	//
	// Repetition is the acknowledgement protocol, and it is deliberate. A
	// result sent once is lost if that single report does not land -- the
	// UI would keep publishing an operation the agent has already done and
	// will now skip forever, a stable state in which both halves are
	// individually working and jointly stuck. Re-sending costs a few
	// hundred bytes a minute and removes that failure entirely, with no new
	// endpoint on either side.
	OperationResults []OperationResult `json:"operation_results,omitempty"`
	// Filesystems is the storage behind the disks above, one entry per
	// distinct directory.
	Filesystems []ReportFilesystem `json:"filesystems,omitempty"`
}

// ReportDomain is one domain's state as the agent found it. Defined here
// rather than reusing pkg/inventory's own type on purpose: this is a wire
// format shared with a separately-versioned program, so it changes only
// when the protocol changes, not when an internal struct is refactored.
type ReportDomain struct {
	Name           string   `json:"name"`
	UUID           string   `json:"uuid,omitempty"`
	Active         bool     `json:"active"`
	Role           string   `json:"role,omitempty"`
	LastCheckpoint string   `json:"last_checkpoint,omitempty"`
	LastSyncUnix   int64    `json:"last_sync_unix,omitempty"`
	FailureCount   int      `json:"failure_count"`
	ReplicaSource  string   `json:"replica_source,omitempty"`
	ReplicaTargets []string `json:"replica_targets,omitempty"`
	// The promotion record, present only on a domain failed over TO.
	// PromotedFrom identifies which source a promotion displaced, which is
	// what lets the control plane work out who must not keep running
	// alongside it.
	PromotedFrom   string `json:"promoted_from,omitempty"`
	PromotedAtUnix int64  `json:"promoted_at_unix,omitempty"`
	PromotedBy     string `json:"promoted_by,omitempty"`
	PromotionMode  string `json:"promotion_mode,omitempty"`
	// Disks is what this domain occupies on this host's storage: the
	// allocated figure, not the apparent one, because that is what a copy
	// actually costs. Paired with Report.Filesystems it answers "is there
	// room to keep the old copy?" before an inversion rather than after.
	Disks      []ReportDisk `json:"disks,omitempty"`
	Status     string       `json:"status"`
	Reasons    []string     `json:"reasons,omitempty"`
	AgeSeconds int64        `json:"age_seconds"`
}

// SendReport uploads an inventory report.
func (c *Client) SendReport(ctx context.Context, r Report) error {
	_, err := c.do(ctx, http.MethodPost, c.agentURL("report"), c.Creds.Token, r, nil, nil)
	return err
}

// PollConfig asks for the current configuration, long-polling: the UI holds
// the request open for up to wait before answering, so an operator's change
// reaches the agent within a second or two without the hypervisor accepting
// any inbound connection.
//
// Returns ErrUnchanged when the UI answers 304, meaning the cached
// configuration is still current.
func (c *Client) PollConfig(ctx context.Context, etag string, wait time.Duration) (UIConfig, string, error) {
	u := fmt.Sprintf("%s?wait=%d", c.agentURL("config"), int(wait.Seconds()))
	headers := map[string]string{}
	if etag != "" {
		headers["If-None-Match"] = etag
	}

	var newETag string
	body, err := c.do(ctx, http.MethodGet, u, c.Creds.Token, nil, headers, func(resp *http.Response) {
		newETag = resp.Header.Get("ETag")
	})
	if err != nil {
		return UIConfig{}, "", err
	}
	var cfg UIConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return UIConfig{}, "", fmt.Errorf("parse configuration response: %w", err)
	}
	return cfg.Normalize(), newETag, nil
}

func (c *Client) agentURL(suffix string) string {
	return c.Base + pathAgents + url.PathEscape(c.Creds.AgentID) + "/" + suffix
}

// do performs one request, translating HTTP status into the errors above.
// inspect, when non-nil, is called with the response before its body is
// consumed, for headers the caller needs.
func (c *Client) do(ctx context.Context, method, u, token string, payload any, headers map[string]string, inspect func(*http.Response)) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer resp.Body.Close()
	if inspect != nil {
		inspect(resp)
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, ErrUnchanged
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, ErrRevoked
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Bounded: a compromised or malfunctioning UI must not be able to
		// exhaust an agent's memory with an unbounded response body.
		return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	default:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s %s: %s: %s", method, u, resp.Status, strings.TrimSpace(string(detail)))
	}
}

// ReportDisk mirrors pkg/inventory.DiskInfo on the wire.
type ReportDisk struct {
	Path           string `json:"path"`
	ApparentBytes  int64  `json:"apparent_bytes"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	Missing        bool   `json:"missing,omitempty"`
}

// ReportFilesystem mirrors pkg/inventory.Filesystem on the wire.
//
// One entry per distinct directory holding a VM's disks, not per host: a
// hypervisor commonly spreads VMs across several pools, and "the host has
// 2 TB free" is useless when the VM being inverted lives on the full one.
type ReportFilesystem struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
}
