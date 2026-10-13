// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestInvitationAndServiceIdentityMethods_RequestShape pins each #1508 route's method, escaped path,
// query and body, and that the answer decodes into the SDK type.
func TestInvitationAndServiceIdentityMethods_RequestShape(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	desc, sponsor, suspended := "nightly", "alice", true
	for _, tc := range []struct {
		name       string
		status     int
		answer     string
		call       func(c *client.Client) error
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   string
	}{
		{"CreateInvitation", 201, `{"id":"` + id.String() + `","target":"a/b","state":"pending"}`,
			func(c *client.Client) error {
				_, err := c.CreateInvitation(context.Background(), "a/b", client.CreateInvitationRequest{ExpiresInSeconds: 60, TokenName: "ci", ReplacesTokenID: &id})
				return err
			}, "POST", "/api/v1/people/a%2Fb/invitations", "", `{"expires_in_seconds":60,"token_name":"ci","replaces_token_id":"` + id.String() + `"}`},
		{"ListInvitations", 200, `[{"id":"` + id.String() + `","state":"claimed"}]`,
			func(c *client.Client) error {
				got, err := c.ListInvitations(context.Background(), client.InvitationClaimed, client.ListOpts{Limit: 5})
				if err == nil && (len(got) != 1 || got[0].State != client.InvitationClaimed) {
					t.Errorf("ListInvitations = %+v", got)
				}
				return err
			}, "GET", "/api/v1/invitations", "state=claimed&limit=5", ""},
		{"ListPersonInvitations", 200, `[]`,
			func(c *client.Client) error {
				_, err := c.ListPersonInvitations(context.Background(), "a/b", "")
				return err
			}, "GET", "/api/v1/people/a%2Fb/invitations", "", ""},
		{"ListMyInvitations", 200, `[]`,
			func(c *client.Client) error { _, err := c.ListMyInvitations(context.Background()); return err },
			"GET", "/api/v1/me/invitations", "", ""},
		{"RevokeInvitation", 204, ``,
			func(c *client.Client) error { return c.RevokeInvitation(context.Background(), id) },
			"DELETE", "/api/v1/invitations/" + id.String(), "", ""},
		{"ClaimInvitation", 201, `{"id":"` + id.String() + `","token":"wdy_secret","name":"ci"}`,
			func(c *client.Client) error {
				got, err := c.ClaimInvitation(context.Background(), id, client.ClaimInvitationRequest{Name: "ci", TTLSeconds: 3600})
				if err == nil && got.Token != "wdy_secret" {
					t.Errorf("ClaimInvitation token = %q, want the plaintext", got.Token)
				}
				return err
			}, "POST", "/api/v1/me/invitations/" + id.String() + "/claim", "", `{"name":"ci","ttl_seconds":3600}`},
		{"CreateServiceIdentity", 201, `{"name":"ci-nightly","state":"active"}`,
			func(c *client.Client) error {
				_, err := c.CreateServiceIdentity(context.Background(), client.CreateServiceIdentityRequest{Name: "ci-nightly", Sponsor: "alice", UserType: "member"})
				return err
			}, "POST", "/api/v1/service-identities", "", `{"name":"ci-nightly","description":"","sponsor":"alice","user_type":"member"}`},
		{"ListServiceIdentities", 200, `[{"name":"ci-nightly","state":"sponsor_inactive"}]`,
			func(c *client.Client) error {
				got, err := c.ListServiceIdentities(context.Background(), client.ListOpts{Offset: 2})
				if err == nil && (len(got) != 1 || got[0].State != client.ServiceIdentitySponsorInactive) {
					t.Errorf("ListServiceIdentities = %+v", got)
				}
				return err
			}, "GET", "/api/v1/service-identities", "offset=2", ""},
		{"GetServiceIdentity", 200, `{"name":"ci nightly"}`,
			func(c *client.Client) error {
				_, err := c.GetServiceIdentity(context.Background(), "ci nightly")
				return err
			},
			"GET", "/api/v1/service-identities/ci%20nightly", "", ""},
		{"UpdateServiceIdentity", 200, `{"name":"ci-nightly"}`,
			func(c *client.Client) error {
				_, err := c.UpdateServiceIdentity(context.Background(), "ci-nightly", client.UpdateServiceIdentityRequest{Description: &desc, Sponsor: &sponsor, Suspended: &suspended})
				return err
			}, "PATCH", "/api/v1/service-identities/ci-nightly", "", `{"description":"nightly","sponsor":"alice","suspended":true}`},
		{"RetireServiceIdentity", 200, `{"name":"ci-nightly","state":"retired"}`,
			func(c *client.Client) error {
				_, err := c.RetireServiceIdentity(context.Background(), "ci-nightly")
				return err
			},
			"DELETE", "/api/v1/service-identities/ci-nightly", "", ""},
		{"ListMyServiceIdentities", 200, `[]`,
			func(c *client.Client) error { _, err := c.ListMyServiceIdentities(context.Background()); return err },
			"GET", "/api/v1/me/service-identities", "", ""},
		{"MintServiceToken", 201, `{"id":"` + id.String() + `","token":"wdy_secret","principal":"service:ci-nightly"}`,
			func(c *client.Client) error {
				got, err := c.MintServiceToken(context.Background(), "ci-nightly", client.MintServiceTokenRequest{Name: "deploy", TTLSeconds: 86400})
				if err == nil && got.Token != "wdy_secret" {
					t.Errorf("MintServiceToken token = %q, want the plaintext", got.Token)
				}
				return err
			}, "POST", "/api/v1/me/service-identities/ci-nightly/tokens", "", `{"name":"deploy","ttl_seconds":86400}`},
		{"ListServiceTokens", 200, `[{"id":"` + id.String() + `","name":"deploy"}]`,
			func(c *client.Client) error {
				got, err := c.ListServiceTokens(context.Background(), "ci-nightly")
				if err == nil && (len(got) != 1 || got[0].Token != "") {
					t.Errorf("ListServiceTokens = %+v", got)
				}
				return err
			}, "GET", "/api/v1/me/service-identities/ci-nightly/tokens", "", ""},
		{"RevokeServiceToken", 204, ``,
			func(c *client.Client) error { return c.RevokeServiceToken(context.Background(), "ci-nightly", id) },
			"DELETE", "/api/v1/me/service-identities/ci-nightly/tokens/" + id.String(), "", ""},
		{"MoveToken", 200, `{"token_id":"` + id.String() + `","service_identity":"ci-nightly","disposition":"linked_to_service_identity"}`,
			func(c *client.Client) error {
				got, err := c.MoveToken(context.Background(), id, "ci-nightly")
				if err == nil && got.Disposition != client.TokenLinkedToService {
					t.Errorf("MoveToken disposition = %q", got.Disposition)
				}
				return err
			}, "POST", "/api/v1/tokens/" + id.String() + "/move", "", `{"service_identity":"ci-nightly"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotQuery, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotMethod, gotPath, gotQuery, gotBody = r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(b)
				checkAuth(t, r)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.answer)
			}))
			defer srv.Close()
			if err := tc.call(newTestClient(srv)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath || gotQuery != tc.wantQuery {
				t.Errorf("request = %s %s?%s, want %s %s?%s", gotMethod, gotPath, gotQuery, tc.wantMethod, tc.wantPath, tc.wantQuery)
			}
			if tc.wantBody == "" && gotBody != "" || tc.wantBody != "" && gotBody != tc.wantBody {
				t.Errorf("body = %q, want %q", gotBody, tc.wantBody)
			}
		})
	}
}

// TestInvitationAndServiceIdentityMethods_DecodeReasons pins that a refusal surfaces the machine reason
// the design names, so a script branches on it and never on the sentence.
func TestInvitationAndServiceIdentityMethods_DecodeReasons(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this service identity is suspended", "reason": "service_identity_inactive"})
	}))
	defer srv.Close()
	_, err := newTestClient(srv).MintServiceToken(context.Background(), "ci-nightly", client.MintServiceTokenRequest{Name: "x"})
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Reason != "service_identity_inactive" || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v, want an APIError with reason service_identity_inactive", err)
	}
}

// TestListPersonInvitationsAndServiceListsSurfaceTruncation pins X-Wardyn-Truncated on the Page forms
// the CLI warns from.
func TestListPersonInvitationsAndServiceListsSurfaceTruncation(t *testing.T) {
	c := r3bTruncatedServer(t, true)
	ctx := context.Background()
	if _, tr, err := c.ListPersonInvitationsPage(ctx, "alice", ""); err != nil || !tr {
		t.Errorf("ListPersonInvitationsPage = truncated %v, err %v", tr, err)
	}
	if _, tr, err := c.ListServiceIdentitiesPage(ctx); err != nil || !tr {
		t.Errorf("ListServiceIdentitiesPage = truncated %v, err %v", tr, err)
	}
	if _, tr, err := c.ListServiceTokensPage(ctx, "ci"); err != nil || !tr {
		t.Errorf("ListServiceTokensPage = truncated %v, err %v", tr, err)
	}
}
