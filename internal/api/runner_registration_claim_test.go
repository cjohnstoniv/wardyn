// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A refused claim of the owner's own runner names why (R2-0012). Another person's runner and an absent
// one stay one indistinguishable answer.
func TestRunnerClaimRefusalNamesTheCause(t *testing.T) {
	const generic = "runner claim does not match the owner or fingerprint"
	for _, tc := range []struct {
		name, sub, fp, orgURL string
		state                 types.RunnerState
		age                   time.Duration
		missing               bool
		want                  string
	}{
		{name: "another person's runner", sub: "bob", fp: "fingerprint", state: types.RunnerUnclaimed, want: generic},
		{name: "another person's revoked runner", sub: "bob", fp: "fingerprint", state: types.RunnerRevoked, want: generic},
		{name: "another person's runner on a different org url", sub: "bob", fp: "fingerprint", state: types.RunnerUnclaimed, orgURL: "https://other.example.com", want: generic},
		{name: "no such runner", sub: "alice", fp: "fingerprint", state: types.RunnerUnclaimed, missing: true, want: generic},
		{name: "already claimed", sub: "alice", fp: "fingerprint", state: types.RunnerClaimed, want: "already been claimed"},
		{name: "revoked", sub: "alice", fp: "fingerprint", state: types.RunnerRevoked, want: "was revoked"},
		{name: "registration expired", sub: "alice", fp: "fingerprint", state: types.RunnerUnclaimed, age: 48 * time.Hour, want: "registration expired"},
		{name: "org url changed", sub: "alice", fp: "fingerprint", state: types.RunnerUnclaimed, orgURL: "https://other.example.com", want: "different organisation URL"},
		{name: "wrong fingerprint", sub: "alice", fp: "wrong", state: types.RunnerUnclaimed, want: "fingerprint does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, f, _ := newRunnerRegistrationServer(t)
			id := uuid.New()
			orgURL := srv.cfg.RunnerOrgURL
			if tc.orgURL != "" {
				orgURL = tc.orgURL
			}
			if !tc.missing {
				f.rows[id] = types.Runner{ID: id, Owner: "alice", KeyFingerprint: "fingerprint", State: tc.state, CreatedAt: time.Now().UTC().Add(-tc.age), OrgURLSHA256: federation.OrgURLSHA256(orgURL)}
			}
			body, _ := json.Marshal(types.RunnerClaimRequest{Fingerprint: tc.fp})
			cookie := memberModeSSOSession(t, tc.sub, tc.sub+"@example.com", oidc.RoleUser, false)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/me/runners/"+id.String()+"/claim", cookie, string(body))
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("claim = %d %s, want 403 containing %q", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}
