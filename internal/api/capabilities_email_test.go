// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL on (UnverifiedEmailCapGrantsMatch false), an allow written
// for the caller's email matches only when sign-in vouches for the email; a deny on it binds either way,
// and the switch off gives back the old match.
func TestCapAllowed_EmailAllowFollowsTheEmailTrustRule(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	enf := map[string]bool{capEgressHost: true}
	onEmail := grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", allow)
	for _, tc := range []struct {
		name          string
		grants        []types.CapabilityGrant
		on, emailKeys bool
		want          bool
	}{
		{"switch off: an allow on the unverified email still matches", []types.CapabilityGrant{onEmail}, false, false, true},
		{"switch on: an allow on the unverified email grants nothing", []types.CapabilityGrant{onEmail}, true, false, false},
		{"switch on, email keys opted in: the email is trusted", []types.CapabilityGrant{onEmail}, true, true, true},
		{"switch on: an allow on the sub matches", []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow)}, true, false, true},
		{"switch on: a deny on the unverified email still binds", []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow),
			grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", deny),
		}, true, false, false},
		{"switch off: a deny on the unverified email still binds", []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow),
			grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", deny),
		}, false, false, false},
	} {
		srv := capServer(&capStore{grants: tc.grants, enf: enf})
		srv.cfg.UnverifiedEmailCapGrantsMatch, srv.cfg.AllowEmailMappings = !tc.on, tc.emailKeys
		got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org")
		if err != nil || got != tc.want {
			t.Errorf("%s: capAllowed = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL unset is on: with the zero Config an allow on the caller's
// unverified email grants nothing, while allows on the sub, a group, the user type and everyone still
// grant, and a deny on the email still binds.
func TestCapAllowed_EmailDefaultOnLeavesOtherSubjectsAlone(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	enf := map[string]bool{capEgressHost: true}
	ctx := withOIDCUserType(memberCtx([]string{"eng"}), "standard")
	for _, tc := range []struct {
		name   string
		grants []types.CapabilityGrant
		want   bool
	}{
		{"an allow on the unverified email", []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", allow)}, false},
		{"an allow on the sub", []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow)}, true},
		{"an allow on a group", []types.CapabilityGrant{grant(types.CapabilitySubjectGroup, "eng", capEgressHost, "pypi.org", allow)}, true},
		{"an allow on the user type", []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, "standard", capEgressHost, "pypi.org", allow)}, true},
		{"an allow on everyone", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org", allow)}, true},
		{"a deny on the unverified email over an allow on everyone", []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capEgressHost, "pypi.org", allow),
			grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", deny),
		}, false},
	} {
		srv := capServer(&capStore{grants: tc.grants, enf: enf})
		if got, err := srv.capAllowed(ctx, capEgressHost, "pypi.org"); err != nil || got != tc.want {
			t.Errorf("default, %s: capAllowed = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// Boot WARNs with the count of user allow grants keyed to an email when they grant nothing (switch on,
// email not vouched for), naming both remedies; it stays silent with none, with the switch off, or with
// email keys opted in.
func TestLogGrantSwitches_WarnsOnEmailAllowRowsThatNoLongerGrant(t *testing.T) {
	const warn = "keyed to a person's email grant nothing"
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	rows := []types.CapabilityGrant{
		grant(types.CapabilitySubjectUser, "alice@corp.example", capEgressHost, "pypi.org", allow),
		grant(types.CapabilitySubjectUser, "bob@corp.example", "image", "python", allow),
		grant(types.CapabilitySubjectUser, "carol@corp.example", capEgressHost, "pypi.org", deny),
		grant(types.CapabilitySubjectUser, capSub, capEgressHost, "pypi.org", allow),
		grant(types.CapabilitySubjectGroup, "eng@corp.example", capEgressHost, "pypi.org", allow),
	}
	for _, tc := range []struct {
		name           string
		grants         []types.CapabilityGrant
		off, emailKeys bool
		wantWarn       bool
	}{
		{"default with two email allow rows", rows, false, false, true},
		{"default with none", rows[2:], false, false, false},
		{"switch off", rows, true, false, false},
		{"email keys opted in", rows, false, true, false},
	} {
		srv := capServer(&capStore{grants: tc.grants})
		srv.cfg.UnverifiedEmailCapGrantsMatch, srv.cfg.AllowEmailMappings = tc.off, tc.emailKeys
		var logged lockedBuffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
		srv.LogGrantSwitches(context.Background())
		slog.SetDefault(prev)
		out := logged.String()
		if !strings.Contains(out, "WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL="+map[bool]string{true: "false", false: "true"}[tc.off]) {
			t.Errorf("%s: the switch state is not logged: %q", tc.name, out)
		}
		switch got := strings.Count(out, warn); {
		case tc.wantWarn && (got != 1 || !strings.Contains(out, "level=WARN") || !strings.Contains(out, "rows=2") ||
			!strings.Contains(out, "user id or a group") || !strings.Contains(out, "WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL=false")):
			t.Errorf("%s: want one WARN with rows=2 and both remedies, logged %q", tc.name, out)
		case !tc.wantWarn && got != 0:
			t.Errorf("%s: want no warning, logged %q", tc.name, out)
		}
	}
}

// The list filter reads the same email trust rule as capAllowed: an allow on the caller's unverified email
// keeps nothing with the switch on, and keeps the value with the switch off or email keys opted in.
func TestCapFilter_EmailAllowFollowsTheEmailTrustRule(t *testing.T) {
	const p = "5b0f2f6e-5f5e-4c53-9d83-0c5a1d3b7a11"
	for _, tc := range []struct {
		name               string
		switchOff, emailOK bool
		want               []string
	}{
		{"switch on", false, false, []string{}},
		{"switch off", true, false, []string{p}},
		{"email keys opted in", false, true, []string{p}},
	} {
		srv := capServer(&capStore{
			grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capPolicy, p, types.CapabilityAllow)},
			enf:    map[string]bool{capPolicy: true},
		})
		srv.cfg.UnverifiedEmailCapGrantsMatch, srv.cfg.AllowEmailMappings = tc.switchOff, tc.emailOK
		got, err := srv.capFilter(memberCtx(nil), capPolicy, []string{p})
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: capFilter = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// The email trust rule holds for every capability kind, not only egress hosts, and a deny on the email
// binds either way. A kind added to capabilityKinds without a value here fails by name.
func TestCapAllowed_EmailRuleHoldsForEveryKind(t *testing.T) {
	const id = "5b0f2f6e-5f5e-4c53-9d83-0c5a1d3b7a11"
	values := map[string]string{
		capEgressHost: "pypi.org", capSecret: "s1", capImage: "ghcr.io/acme/x:1", capFeature: "service_attach",
		capModelProvider: "mp1", capWorkspaceProvider: "gh1", capAgent: "claude-code",
		capWorkspace: id, capPolicy: id, capComponent: id,
	}
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	for _, kind := range capabilityKinds {
		value, ok := values[kind]
		if !ok {
			t.Errorf("capability kind %q has no value in this test: add one so the email rule is pinned for it", kind)
			continue
		}
		for _, tc := range []struct {
			name      string
			grants    []types.CapabilityGrant
			switchOff bool
			want      bool
		}{
			{"allow on the email, switch on", []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, kind, value, allow)}, false, false},
			{"allow on the email, switch off", []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, kind, value, allow)}, true, true},
			{"deny on the email over an allow on the sub, switch on", []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capSub, kind, value, allow),
				grant(types.CapabilitySubjectUser, capEmail, kind, value, deny),
			}, false, false},
			{"deny on the email over an allow on the sub, switch off", []types.CapabilityGrant{
				grant(types.CapabilitySubjectUser, capSub, kind, value, allow),
				grant(types.CapabilitySubjectUser, capEmail, kind, value, deny),
			}, true, false},
		} {
			srv := capServer(&capStore{grants: tc.grants, enf: map[string]bool{kind: true}})
			srv.cfg.UnverifiedEmailCapGrantsMatch = tc.switchOff
			if got, err := srv.capAllowed(memberCtx(nil), kind, value); err != nil || got != tc.want {
				t.Errorf("%s, %s: capAllowed = %v, %v; want %v", kind, tc.name, got, err, tc.want)
			}
		}
	}
}

// emailTrusted's other arm: sign-in that enforces email_verified vouches for the email, so an allow on it
// grants with the switch on and no email-keys opt-in.
func TestCapAllowed_TrustedEmailMatchesThroughOIDC(t *testing.T) {
	srv := capServer(&capStore{
		grants: []types.CapabilityGrant{grant(types.CapabilitySubjectUser, capEmail, capEgressHost, "pypi.org", types.CapabilityAllow)},
		enf:    map[string]bool{capEgressHost: true},
	})
	if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || got {
		t.Fatalf("without OIDC enforcing email_verified: capAllowed = %v, %v; want false", got, err)
	}
	srv.cfg.OIDC = newAccessAuth(t, nil, "", nil, nil, func(c *oidc.Config) { c.RequireEmailVerified = true })
	if got, err := srv.capAllowed(memberCtx(nil), capEgressHost, "pypi.org"); err != nil || !got {
		t.Errorf("with email_verified enforced: capAllowed = %v, %v; want true", got, err)
	}
}
