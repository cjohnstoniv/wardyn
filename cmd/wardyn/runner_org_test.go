// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestRunnerOrgCommands(t *testing.T) {
	id := uuid.New()
	msrv := newCmdServer(t, http.StatusCreated, types.RunnerRegistrationToken{ID: id, Token: "wdr_secret", OwnerKind: types.RunnerOwnerOrg, ExpiresAt: time.Now().Add(72 * time.Hour)})
	out, err := execCmdCapture(t, "runner", "org-token", "mint", "--name", "cluster-a", "--url", msrv.URL, "--token", "t")
	var mint types.RunnerOrgTokenRequest
	if req := msrv.last(); err != nil || req.method != http.MethodPost || req.path != "/api/v1/runners/org-tokens" || json.Unmarshal(req.body, &mint) != nil ||
		mint.Name != "cluster-a" || !strings.Contains(out, "wdr_secret") {
		t.Fatalf("org-token mint: %v %+v %+v\n%s", err, req, mint, out)
	}
	if err := execCmd(t, "runner", "org-token", "mint", "--url", msrv.URL, "--token", "t"); err == nil {
		t.Fatal("an org mint without --name was accepted")
	}

	csrv := newCmdServer(t, http.StatusOK, types.RunnerRegistration{RunnerID: id, State: types.RunnerClaimed})
	out, err = execCmdCapture(t, "runner", "claim-org", id.String(), "--fingerprint", "abcd", "--url", csrv.URL, "--token", "t")
	var claim types.RunnerClaimRequest
	if req := csrv.last(); err != nil || req.path != "/api/v1/runners/"+id.String()+"/claim-org" || json.Unmarshal(req.body, &claim) != nil ||
		claim.Fingerprint != "abcd" || !strings.Contains(out, "Claimed organization runner "+id.String()) {
		t.Fatalf("claim-org: %v %+v %+v\n%s", err, req, claim, out)
	}

	tsrv := newCmdServer(t, http.StatusOK, types.RunnerView{ID: id, Name: "laptop", TrustClass: types.RunnerTrustManagedDevice})
	out, err = execCmdCapture(t, "runner", "trust", id.String(), "managed_device", "--reason", "enrolled in device management", "--url", tsrv.URL, "--token", "t")
	var trust types.RunnerTrustRequest
	if req := tsrv.last(); err != nil || req.method != http.MethodPut || req.path != "/api/v1/runners/"+id.String()+"/trust" || json.Unmarshal(req.body, &trust) != nil ||
		trust.TrustClass != types.RunnerTrustManagedDevice || trust.Reason != "enrolled in device management" || !strings.Contains(out, "trust class is managed_device") {
		t.Fatalf("trust: %v %+v %+v\n%s", err, req, trust, out)
	}
	if err := execCmd(t, "runner", "trust", id.String(), "trusted", "--reason", "r", "--url", tsrv.URL, "--token", "t"); err == nil ||
		!strings.Contains(err.Error(), "personal or managed_device") || strings.Contains(err.Error(), "or org_cluster") {
		t.Errorf("an unknown trust class was refused with %v, want personal or managed_device only", err)
	}
	n := len(tsrv.reqs)
	for _, bad := range [][]string{
		{"claim-org", id.String()},
		{"claim-org", "not-a-uuid", "--fingerprint", "abcd"},
		{"trust", id.String(), "managed_device"},
		{"trust", id.String(), "trusted", "--reason", "r"},
		{"trust", "not-a-uuid", "personal", "--reason", "r"},
	} {
		if err := execCmd(t, append(append([]string{"runner"}, bad...), "--url", tsrv.URL, "--token", "t")...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(tsrv.reqs) != n {
		t.Error("an invalid org runner command reached the server")
	}
	held := newCmdServer(t, http.StatusAccepted, map[string]any{"pending_change": map[string]any{"id": uuid.NewString(), "state": "pending"}})
	out, err = execCmdCapture(t, "runner", "trust", id.String(), "personal", "--reason", "r", "--url", held.URL, "--token", "t")
	if err != nil || !strings.Contains(out, "pending approval: 1 change(s) stored") {
		t.Errorf("a held trust write = %v, printed %q, want exit 0 saying it waits", err, out)
	}
}
