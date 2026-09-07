// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F245 residue: the first fix projected the workspace WRAPPER (Sources,
// BaseImage, Requirements) and left Workspace.Profile — the scan result that
// travels in the SAME response — intact. The profile republishes every axis the
// wrapper was just stripped of, under its own keys, so the redaction was
// cosmetic for any scanned workspace:
//
//	required_secrets[].name    == the `secret:<NAME>` requirement key
//	secret_files_present[]     == the host path Sources[].Path was blanked for
//	leak_findings[].path       == the same
//	egress_domains[]           == the `egress:<host>` requirement keys
//	suggested_egress[]         == internal hosts the member tier decides nothing about
//
// TestWorkspaceReadRedaction now carries a scanned profile in its fixture and is
// the primary pin. These are the assertions its forbidden/required lists do not
// name: the per-key projection, both counterfactual directions, and fail-closed.

// f245ProfileOf decodes the profile out of one workspace read.
func f245ProfileOf(t *testing.T, srv *Server, path string, session *http.Cookie) map[string]json.RawMessage {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, path, session, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, w.Code, w.Body.String())
	}
	var ws struct {
		Profile map[string]json.RawMessage `json:"profile"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode %s: %v; body=%s", path, err, w.Body.String())
	}
	return ws.Profile
}

// TestF245_ScannedProfileIsProjectedPerTier walks the three tiers key by key.
func TestF245_ScannedProfileIsProjectedPerTier(t *testing.T) {
	const owner = "sub-ws-owner"
	for _, tc := range []struct {
		name    string
		ownedBy string
		session *http.Cookie
		gone    []string
		kept    []string
	}{
		{
			name:    "a plain member reading an operator-owned workspace",
			ownedBy: "",
			session: ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember),
			// The host axis AND the internal egress hosts: this tier decides
			// nothing about either.
			gone: []string{"required_secrets", "secret_files_present", "leak_findings", "egress_domains", "suggested_egress"},
			// The row stays USEFUL — over-redaction breaks the product the
			// projection exists to preserve.
			kept: []string{"languages", "confidence", "source"},
		},
		{
			name:    "a security admin reading a foreign member-owned workspace",
			ownedBy: owner,
			session: ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin),
			// The HOST axis goes; the EGRESS axis stays, because rewriting this
			// workspace's approved/denied egress is what the tier is widened FOR.
			gone: []string{"required_secrets", "secret_files_present", "leak_findings"},
			kept: []string{"egress_domains", "suggested_egress", "languages"},
		},
		{
			name:    "the workspace's own owner",
			ownedBy: owner,
			session: ssoSession(t, owner, "owner@corp.example", oidc.RoleMember),
			// Nothing is withheld from the person who authored these paths.
			kept: []string{"required_secrets", "secret_files_present", "leak_findings", "egress_domains", "suggested_egress", "languages"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st := newTopologyWorkspaceServer(t, tc.ownedBy)
			path := "/api/v1/workspaces/" + st.ws.ID.String()
			got := f245ProfileOf(t, srv, path, tc.session)
			for _, k := range tc.gone {
				if _, present := got[k]; present {
					t.Errorf("GET %s: profile.%s survived the projection (%s) — the same datum the wrapper was stripped of, republished by the scan result", path, k, got[k])
				}
			}
			for _, k := range tc.kept {
				if _, present := got[k]; !present {
					t.Errorf("GET %s: profile.%s was dropped; this tier needs it", path, k)
				}
			}
		})
	}
}

// TestF245_ListArmProjectsTheProfileToo: the list is the route a member actually
// loads, and it hands out the same document N times.
func TestF245_ListArmProjectsTheProfileToo(t *testing.T) {
	srv, _ := newTopologyWorkspaceServer(t, "")
	member := ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces", member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /workspaces = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, bad := range []string{r3TopologySecret, r3TopologyLeakPath, r3TopologyEgress, r3TopologySuggested, "required_secrets", "leak_findings"} {
		if strings.Contains(body, bad) {
			t.Errorf("GET /workspaces leaked %q through the scanned profile\nbody=%s", bad, body)
		}
	}
	if !strings.Contains(body, "payments") {
		t.Errorf("GET /workspaces dropped the workspace name; the list must stay usable\nbody=%s", body)
	}
}

// TestF245_UnparseableProfileFailsClosed: a blob whose fields cannot be
// inspected cannot be certified free of the axes above, so it is withheld rather
// than shipped unread. Pinned because the tempting alternative — pass it through
// on a decode error — reopens the finding for any profile shape this package
// does not parse.
func TestF245_UnparseableProfileFailsClosed(t *testing.T) {
	srv, st := newTopologyWorkspaceServer(t, "")
	st.ws.Profile = json.RawMessage(`{"egress_domains":[` + r3TopologyEgress) // truncated on purpose
	member := ssoSession(t, "sub-plain-member", "m@corp.example", oidc.RoleMember)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/workspaces/"+st.ws.ID.String(), member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), r3TopologyEgress) {
		t.Errorf("an undecodable profile was shipped verbatim to a member: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payments") {
		t.Errorf("the rest of the document went with it; only the profile is withheld: %s", w.Body.String())
	}
}

// f245Unused keeps the types import honest if the fixture shape changes.
var _ = types.Workspace{}
