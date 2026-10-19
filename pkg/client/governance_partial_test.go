// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// An apply that fails after a write was held still hands back the held change: errors.As finds a
// *PendingApprovalError with its id beside the failure, so the caller can approve or reject it.
func TestApplyGovernance_FailureAfterAHeldWriteKeepsTheHeldChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance":
			writeJSON(w, http.StatusOK, client.GovernanceDocument{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/governance/profiles":
			var req client.GovernanceProfileRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Name == "held" {
				writeJSON(w, http.StatusAccepted, pendingChange("create", "governance_profile", req.Name))
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := client.New(srv.URL, "tok").ApplyGovernance(context.Background(), client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{Name: "held"}, {Name: "broken"}}}, false)
	var pe *client.PendingApprovalError
	if !errors.As(err, &pe) || len(pe.Changes) != 1 || pe.Changes[0].TargetKey != "held" {
		t.Fatalf("ApplyGovernance err = %v, want the failure joined with the held change", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("ApplyGovernance err = %v, want the failed write named too", err)
	}
}

// A decided change keeps who decided it, when, and a rejection's reason through the SDK, so
// changes list --json prints them.
func TestGovernanceChange_KeepsTheDecision(t *testing.T) {
	decided := time.Now().UTC().Truncate(time.Second)
	at := func(d time.Duration) string { return decided.Add(d).Format(time.RFC3339) }
	in := `{"id":"00000000-0000-0000-0000-000000000001","target_kind":"governance_profile","op":"create",` +
		`"target_key":"p","state":"rejected","proposed_by":"alice","proposed_at":"` + at(-24*time.Hour) + `",` +
		`"expires_at":"` + at(48*time.Hour) + `","diff":{},"decided_by":"bob","decided_at":"` + at(0) + `","reason":"too wide"}`
	var c client.GovernanceChange
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"decided_by":"bob"`, `"decided_at":"` + at(0) + `"`, `"reason":"too wide"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("round trip = %s, want %s", out, want)
		}
	}
}

// An external module, which cannot import internal/types, declares the egress choice and switches
// on an edit's kind and operation with the SDK's own names.
func TestOverrideItems_SpeakTheSDKsNames(t *testing.T) {
	var mode client.FirstUseMode = client.FirstUseWaitForReview
	items := client.RunOverrides{Egress: &client.RunEgressOverrides{FirstUseApproval: mode}}.Items()
	if len(items) != 1 || items[0].Kind != client.OverrideFirstUseApproval || items[0].Op != client.OverrideSet {
		t.Errorf("Items() = %+v, want one first_use_approval set", items)
	}
}
