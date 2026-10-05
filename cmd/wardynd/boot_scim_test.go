// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

const (
	scimTestIssuer = "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
	scimTestToken  = "scim-test-token-0123456789abcdef0123456789"
	scimTestNext   = "scim-next-token-0123456789abcdef0123456789"
	scimTestAdmin  = "admin-test-token-0123456789abcdef0123456789"
)

func scimFlags(token, next, issuer, admin string) *bootFlags {
	return &bootFlags{scimToken: &token, scimTokenNext: &next, oidcIssuer: &issuer, adminToken: &admin}
}

// SCIM mounts only on a configuration where a suspension cannot silently miss: OIDC on a single-tenant
// commercial-cloud Entra issuer, TLS, and tokens that are long, distinct from each other and from the
// admin credential. Every other shape refuses boot, naming the setting and never a token's value.
func TestSCIMConfigRefusals(t *testing.T) {
	served := tlsPosture{tlsEnabled: true, secureCookies: true}
	for _, c := range []struct {
		name    string
		f       *bootFlags
		posture tlsPosture
		want    string // "" = accepted
	}{
		{name: "off", f: scimFlags("", "", "", ""), posture: served},
		{name: "a single-tenant entra issuer over tls", f: scimFlags(scimTestToken, "", scimTestIssuer, scimTestAdmin), posture: served},
		{name: "with a rotation token", f: scimFlags(scimTestToken, scimTestNext, scimTestIssuer, scimTestAdmin), posture: served},
		{name: "a tls-terminating proxy counts", f: scimFlags(scimTestToken, "", scimTestIssuer, scimTestAdmin), posture: tlsPosture{secureCookies: true}},
		{name: "no oidc", f: scimFlags(scimTestToken, "", "", scimTestAdmin), posture: served, want: "OIDC is not configured"},
		{name: "the common issuer", f: scimFlags(scimTestToken, "", "https://login.microsoftonline.com/common/v2.0", scimTestAdmin), posture: served, want: "no single Entra tenant"},
		{name: "the common issuer in any case", f: scimFlags(scimTestToken, "", "https://login.microsoftonline.com/Common/v2.0", scimTestAdmin), posture: served, want: "no single Entra tenant"},
		{name: "the organizations issuer", f: scimFlags(scimTestToken, "", "https://login.microsoftonline.com/organizations/v2.0", scimTestAdmin), posture: served, want: "no single Entra tenant"},
		{name: "the consumers issuer", f: scimFlags(scimTestToken, "", "https://login.microsoftonline.com/consumers/v2.0", scimTestAdmin), posture: served, want: "no single Entra tenant"},
		{name: "a non-entra issuer", f: scimFlags(scimTestToken, "", "https://dex.example.com/dex", scimTestAdmin), posture: served, want: "no single Entra tenant"},
		{name: "a sovereign-cloud issuer", f: scimFlags(scimTestToken, "", "https://login.microsoftonline.us/11111111-2222-3333-4444-555555555555/v2.0", scimTestAdmin), posture: served, want: "no single Entra tenant"},
		{name: "no tls", f: scimFlags(scimTestToken, "", scimTestIssuer, scimTestAdmin), posture: tlsPosture{}, want: "TLS"},
		{name: "a short token", f: scimFlags("short-token", "", scimTestIssuer, scimTestAdmin), posture: served, want: "shorter than 32 bytes"},
		{name: "a short rotation token", f: scimFlags(scimTestToken, "short", scimTestIssuer, scimTestAdmin), posture: served, want: "WARDYN_SCIM_TOKEN_NEXT is shorter"},
		{name: "equal to the admin token", f: scimFlags(scimTestAdmin, "", scimTestIssuer, scimTestAdmin), posture: served, want: "equals the admin token"},
		{name: "the rotation token equal to the admin token", f: scimFlags(scimTestToken, scimTestAdmin, scimTestIssuer, scimTestAdmin), posture: served, want: "WARDYN_SCIM_TOKEN_NEXT equals the admin token"},
		{name: "equal to the rotation token", f: scimFlags(scimTestToken, scimTestToken, scimTestIssuer, scimTestAdmin), posture: served, want: "WARDYN_SCIM_TOKEN_NEXT equals WARDYN_SCIM_TOKEN"},
		{name: "a rotation token alone", f: scimFlags("", scimTestNext, scimTestIssuer, scimTestAdmin), posture: served, want: "without WARDYN_SCIM_TOKEN"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := scimConfig(c.f, c.posture)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				if (cfg != nil) != (*c.f.scimToken != "") {
					t.Fatalf("config = %+v for token %q", cfg, *c.f.scimToken)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v does not mention %q", err, c.want)
			}
			for _, secret := range []string{scimTestToken, scimTestNext, scimTestAdmin} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the refusal prints a token's value: %v", err)
				}
			}
		})
	}
}

// The tenant SCIM matches under is the issuer's, the issuer is kept as written, and the demo admin
// token (which is also too short) is refused by name whatever its length.
func TestSCIMConfigDerivesTheTenant(t *testing.T) {
	cfg, err := scimConfig(scimFlags(scimTestToken, scimTestNext, scimTestIssuer, scimTestAdmin), tlsPosture{secureCookies: true})
	if err != nil || cfg.Tenant != "11111111-2222-3333-4444-555555555555" || cfg.Issuer != scimTestIssuer || cfg.Token != scimTestToken || cfg.TokenNext != scimTestNext {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	if _, err := scimConfig(scimFlags(demoAdminToken, "", scimTestIssuer, scimTestAdmin), tlsPosture{secureCookies: true}); err == nil {
		t.Error("the demo admin token was accepted as a SCIM token")
	}
}

// Both SCIM tokens take a _FILE twin, through the one list every secret file goes through.
func TestSCIMTokensTakeFileTwins(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("WARDYN_SCIM_TOKEN_FILE", write("token", scimTestToken))
	t.Setenv("WARDYN_SCIM_TOKEN_NEXT_FILE", write("next", scimTestNext))
	token, next := "", ""
	if err := resolveSecretFiles(secretFileSettings(&bootFlags{scimToken: &token, scimTokenNext: &next})); err != nil {
		t.Fatalf("resolveSecretFiles: %v", err)
	}
	if token != scimTestToken || next != scimTestNext {
		t.Errorf("resolved = %q, %q", token, next)
	}
}

// With a SCIM token set, the session store reads the owner's identity rows in the same statement as the
// cutoff: a deactivated identity, or an authority epoch past the credential's, answers SessionDeactivated;
// a plain cutoff still answers SessionRevoked; IsSessionRevoked ignores the identity rows entirely.
func TestPGSessionStatusReadsTheIdentityRows(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed session-status test")
	}
	ctx := t.Context()
	pool, err := connectAndMigrate(ctx, dsn, "", 30*time.Second, 60*time.Second, false)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	defer pool.Close()
	rev := &pgSessionRevocations{pool: pool}
	st := store.NewPG(pool)
	sub := "sub-status-probe-" + uuid.NewString()
	issued := time.Now().UTC().Add(time.Minute)
	row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: sub, Issuer: "https://idp.example/" + sub}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM principal_identities WHERE id = $1`, row.ID) })

	status := func(epoch int64) oidc.SessionStatus {
		t.Helper()
		got, err := rev.SessionStatus(ctx, sub, "", issued, epoch)
		if err != nil {
			t.Fatalf("SessionStatus: %v", err)
		}
		return got
	}
	if status(0) != oidc.SessionLive || status(-1) != oidc.SessionLive {
		t.Fatal("an active identity at its epoch is not live")
	}
	if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: row.ID, Principal: sub, Principals: []string{sub}}); err != nil {
		t.Fatal(err)
	}
	if status(0) != oidc.SessionDeactivated || status(-1) != oidc.SessionDeactivated || status(1) != oidc.SessionDeactivated {
		t.Error("a deactivated identity is not refused")
	}
	if revoked, err := rev.IsSessionRevoked(ctx, sub, "", issued); err != nil || revoked {
		t.Errorf("IsSessionRevoked read the identity rows: %v, %v", revoked, err)
	}
	if _, err := st.ApplyIdentityUpdate(ctx, row.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if status(0) != oidc.SessionDeactivated {
		t.Error("a credential admitted before the suspension is live after the reactivation")
	}
	if status(1) != oidc.SessionLive || status(-1) != oidc.SessionLive {
		t.Error("a credential at the new epoch, or with no epoch, is refused after the reactivation")
	}
	// A cutoff written for the sub is the plain answer, with identity rows quiet.
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if got, _ := rev.SessionStatus(ctx, sub, "", time.Now().UTC().Add(-time.Hour), 1); got != oidc.SessionRevoked {
		t.Errorf("a cutoff = %v, want SessionRevoked", got)
	}
}
