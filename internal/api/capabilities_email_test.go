// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"
	"testing"

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
