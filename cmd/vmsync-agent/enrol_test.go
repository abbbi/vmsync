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
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A modeless enrolment spends the token and persists the credential, without
// any mode flag and without touching libvirtd (only --once inventories).
func TestRunEnrolOnlySpendsTheTokenAndSavesTheCredential(t *testing.T) {
	var sawAuth, sawPath string
	var sawToken string
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuth = r.Header.Get("Authorization")
		var body enrolRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode enrolment body: %v", err)
		}
		sawToken = body.EnrolmentToken
		json.NewEncoder(w).Encode(enrolResponse{AgentID: "agent-7", Token: "long-lived-token"})
	})

	store := Store{Dir: t.TempDir()}
	cfg := agentConfig{Hostname: "hyper01p", EnrolToken: "one-time-token"}
	if err := runEnrolOnly(context.Background(), c, store, cfg); err != nil {
		t.Fatalf("runEnrolOnly: %v", err)
	}
	if sawPath != pathEnrol {
		t.Errorf("enrolment hit %s, want %s", sawPath, pathEnrol)
	}
	// Enrolment is the one call with no bearer token -- the enrolment token
	// in the body is the credential being spent.
	if sawAuth != "" {
		t.Errorf("enrolment sent Authorization: %q, want none", sawAuth)
	}
	if sawToken != "one-time-token" {
		t.Errorf("enrolment spent token %q, want the one passed in", sawToken)
	}
	creds, ok, err := store.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if !ok {
		t.Fatal("no credential was stored; the next start would need another token")
	}
	if creds.AgentID != "agent-7" || creds.Token != "long-lived-token" {
		t.Errorf("stored credential = %+v, want what the UI issued", creds)
	}
}

// Re-running setup on an enrolled host must not spend anything: with no new
// token the stored credential is reused and the UI is never contacted.
func TestRunEnrolOnlyReusesAStoredCredentialWithoutCallingTheUI(t *testing.T) {
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the UI was contacted though a credential was already stored")
		http.Error(w, "must not happen", http.StatusInternalServerError)
	})

	store := Store{Dir: t.TempDir()}
	if err := store.SaveCredentials(Credentials{AgentID: "agent-7", Token: "long-lived-token", UIBase: c.Base}); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	cfg := agentConfig{Hostname: "hyper01p"}
	if err := runEnrolOnly(context.Background(), c, store, cfg); err != nil {
		t.Fatalf("runEnrolOnly with a stored credential: %v", err)
	}
	if c.Creds.AgentID != "agent-7" || c.Creds.Token != "long-lived-token" {
		t.Errorf("client credential = %+v, want the stored one", c.Creds)
	}
}

// With neither a token nor a stored credential there is nothing to enrol
// with -- the same refusal the daemon path makes, naming the real flag.
func TestRunEnrolOnlyWithoutAnythingToEnrolWithRefuses(t *testing.T) {
	c, _ := stubUI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the UI was contacted though there was nothing to enrol with")
	})
	store := Store{Dir: t.TempDir()}
	cfg := agentConfig{Hostname: "hyper01p"}
	err := runEnrolOnly(context.Background(), c, store, cfg)
	if err == nil {
		t.Fatal("enrolment with no token and no stored credential was accepted")
	}
	if !strings.Contains(err.Error(), "--enrol-token-file") {
		t.Errorf("error %q does not name the flag that would fix it", err)
	}
}
