// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNewSpellingsAreReachable_InvitationsAndServiceIdentities drives every #1508 verb against a stub server and pins the
// request each one makes and what it prints.
func TestNewSpellingsAreReachable_InvitationsAndServiceIdentities(t *testing.T) {
	id := uuid.New()
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	inv := types.Invitation{ID: id, Target: "alice", CreatedBy: "admin", ExpiresAt: at, State: types.InvitationPending, ConsolePath: "/account?invitation=" + id.String()}
	si := types.ServiceIdentity{Name: "ci-nightly", Sponsor: "alice", UserType: "member", CreatedAt: at, State: types.ServiceIdentityActive}
	tok := types.APIToken{ID: id, Name: "deploy", CreatedAt: at}
	for _, tc := range []struct {
		name     string
		args     []string
		status   int
		answer   any
		method   string
		path     string
		query    string
		body     string
		wantOut  string
		wantNone string
	}{
		{"invite", []string{"person", "invite", "alice", "--expires-in", "48h", "--token-name", "ci", "--token-ttl", "24h", "--replaces", id.String()}, 201, inv,
			"POST", "/api/v1/people/alice/invitations", "",
			`{"expires_in_seconds":172800,"token_name":"ci","token_ttl_seconds":86400,"replaces_token_id":"` + id.String() + `"}`, "/account?invitation=" + id.String(), ""},
		{"invitations", []string{"person", "invitations", "alice", "--state", "pending", "--limit", "5"}, 200, []types.Invitation{inv},
			"GET", "/api/v1/people/alice/invitations", "state=pending&limit=5", "", "alice", ""},
		{"invitation revoke", []string{"invitation", "revoke", id.String()}, 204, nil,
			"DELETE", "/api/v1/invitations/" + id.String(), "", "", "revoked invitation " + id.String(), ""},
		{"create", []string{"service-identity", "create", "--name", "ci-nightly", "--sponsor", "alice", "--user-type", "member"}, 201, si,
			"POST", "/api/v1/service-identities", "", `{"name":"ci-nightly","description":"","sponsor":"alice","user_type":"member"}`, "ci-nightly", ""},
		{"list", []string{"service-identity", "list"}, 200, []types.ServiceIdentity{si},
			"GET", "/api/v1/service-identities", "", "", "ci-nightly", ""},
		{"get", []string{"service-identity", "get", "ci-nightly"}, 200, si,
			"GET", "/api/v1/service-identities/ci-nightly", "", "", "alice", ""},
		{"update suspend", []string{"service-identity", "update", "ci-nightly", "--suspend"}, 200, si,
			"PATCH", "/api/v1/service-identities/ci-nightly", "", `{"suspended":true}`, "ci-nightly", ""},
		{"update resume", []string{"service-identity", "update", "ci-nightly", "--resume", "--description", "nightly"}, 200, si,
			"PATCH", "/api/v1/service-identities/ci-nightly", "", `{"description":"nightly","suspended":false}`, "ci-nightly", ""},
		{"retire", []string{"service-identity", "retire", "ci-nightly"}, 200, si,
			"DELETE", "/api/v1/service-identities/ci-nightly", "", "", "ci-nightly", ""},
		{"token list", []string{"service-identity", "token", "list", "ci-nightly"}, 200, []types.APIToken{tok},
			"GET", "/api/v1/me/service-identities/ci-nightly/tokens", "", "", "deploy", ""},
		{"token revoke", []string{"service-identity", "token", "revoke", "ci-nightly", id.String()}, 204, nil,
			"DELETE", "/api/v1/me/service-identities/ci-nightly/tokens/" + id.String(), "", "", "revoked token " + id.String(), ""},
		{"token move", []string{"token", "move", id.String(), "ci-nightly"}, 200, types.TokenMoved{TokenMove: types.TokenMove{TokenID: id, ServiceIdentity: "ci-nightly"}, Disposition: types.TokenLinkedToService},
			"POST", "/api/v1/tokens/" + id.String() + "/move", "", `{"service_identity":"ci-nightly"}`, "linked_to_service_identity", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCmdServer(t, tc.status, tc.answer)
			out, err := execCmdCapture(t, append(tc.args, "--url", srv.URL, "--token", "t")...)
			if err != nil {
				t.Fatalf("wardyn %s: %v", strings.Join(tc.args, " "), err)
			}
			req := srv.last()
			if req.method != tc.method || req.path != tc.path || req.query != tc.query {
				t.Errorf("request = %s %s?%s, want %s %s?%s", req.method, req.path, req.query, tc.method, tc.path, tc.query)
			}
			if tc.body != "" {
				var got, want map[string]any
				if err := json.Unmarshal(req.body, &got); err != nil {
					t.Fatalf("body %q: %v", req.body, err)
				}
				_ = json.Unmarshal([]byte(tc.body), &want)
				if len(got) != len(want) {
					t.Errorf("body = %s, want %s", req.body, tc.body)
				}
				for k, v := range want {
					if got[k] != v {
						t.Errorf("body[%s] = %v, want %v (%s)", k, got[k], v, req.body)
					}
				}
			} else if len(req.body) != 0 {
				t.Errorf("body = %s, want none", req.body)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
		})
	}
}

// TestNewSpellingsAreReachable_InvitationsAndServiceIdentitiesRefuseBeforeSending pins the client-side checks that fire before
// any request is made.
func TestNewSpellingsAreReachable_InvitationsAndServiceIdentitiesRefuseBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"invitation", "revoke", "not-a-uuid"}, "invalid invitation id"},
		{[]string{"person", "invite", "alice", "--replaces", "not-a-uuid"}, "invalid token id"},
		{[]string{"token", "move", "not-a-uuid", "ci"}, "invalid token id"},
		{[]string{"service-identity", "token", "revoke", "ci", "not-a-uuid"}, "invalid token id"},
		{[]string{"service-identity", "update", "ci"}, "name what to change"},
		{[]string{"service-identity", "update", "ci", "--suspend", "--resume"}, "not both"},
	} {
		if err := execCmd(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("wardyn %s = %v, want an error containing %q", strings.Join(tc.args, " "), err, tc.want)
		}
	}
}

// TestNoClaimAndNoServiceTokenMintVerb pins design ruling D16: claiming an invitation and minting a
// service token need a browser session, so the CLI has no verb for either, hidden or not.
func TestNoClaimAndNoServiceTokenMintVerb(t *testing.T) {
	for _, args := range [][]string{
		{"invitation", "claim", uuid.NewString()},
		{"person", "invitation", "claim", uuid.NewString()},
		{"service-identity", "token", "create", "ci"},
		{"service-identity", "token", "mint", "ci"},
		{"service-identity", "mint", "ci"},
		{"token", "create"},
	} {
		err := execCmd(t, args...)
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("wardyn %s = %v, want an unknown-command refusal", strings.Join(args, " "), err)
		}
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			path := sub.CommandPath()
			mintOrClaim := sub.Name() == "claim" || sub.Name() == "mint" || (sub.Name() == "create" && strings.Contains(path, "token"))
			if mintOrClaim && (strings.Contains(path, "invitation") || strings.Contains(path, "service-identity") || strings.HasPrefix(path, "wardyn token")) {
				t.Errorf("%s is registered: a claim or service-token mint needs a browser session", path)
			}
			walk(sub)
		}
	}
	walk(rootCmd())
}

// TestServiceIdentityTokenHelpSaysMintingIsInTheConsole pins the help text the design asks for.
func TestServiceIdentityTokenHelpSaysMintingIsInTheConsole(t *testing.T) {
	out, err := execCmdCapture(t, "service-identity", "token", "--help")
	if err != nil || !strings.Contains(out, "Minting a service token happens in the console") {
		t.Fatalf("help = %v\n%s", err, out)
	}
}
