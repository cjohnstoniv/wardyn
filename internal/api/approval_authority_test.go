// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pins of a unit or group decider's own egress_host grant on an egress_domain approve (#4073), on the
// fixture of deciders_resolve_test.go. Every case but the unenforced one turns egress_host enforcement on and
// grants dcHost to nobody but the run's owner, so a decider the rule names holds no grant on the host.

// authorityFix is newDCFix with a deciders rule naming who for egress_domain, egress_host enforced and dcHost
// granted to the run's owner only.
func authorityFix(t *testing.T, who string) *dcFix {
	t.Helper()
	f := newDCFix(t)
	f.st.setRules(f.team.ID, dcRule(types.DeciderKindEgressDomain, who))
	f.st.enf = map[string]bool{capEgressHost: true}
	f.st.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUser, dcOwner, capEgressHost, dcHost, types.CapabilityAllow)}
	return f
}

// authorityData is the data of the one row of action for ap, failing unless there is exactly one.
func (f *dcFix) authorityData(action string, ap types.ApprovalRequest) (types.AuditEvent, map[string]any) {
	f.t.Helper()
	rows := f.actionRows(action, ap.ID.String())
	if len(rows) != 1 {
		f.t.Fatalf("%s rows for the approval = %d, want 1: %+v", action, len(rows), rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		f.t.Fatalf("%s data %s: %v", action, rows[0].Data, err)
	}
	return rows[0], data
}

// wantNoRows fails when the approval has any row of action.
func (f *dcFix) wantNoRows(action string, ap types.ApprovalRequest) {
	f.t.Helper()
	if rows := f.actionRows(action, ap.ID.String()); len(rows) != 0 {
		f.t.Errorf("%s rows for the approval = %+v, want none", action, rows)
	}
}

// wantAuthorityAlert asserts the approval was approved and carries one approval.authority.alert from actor
// under basis, in audit mode.
func (f *dcFix) wantAuthorityAlert(ap types.ApprovalRequest, actor, basis string) {
	f.t.Helper()
	if got := f.row(ap).State; got != types.ApprovalApproved {
		f.t.Fatalf("state %s, want APPROVED", got)
	}
	ev, data := f.authorityData("approval.authority.alert", ap)
	if ev.Actor != actor || ev.Outcome != "success" || ev.RunID == nil || *ev.RunID != f.run.ID {
		f.t.Errorf("alert row actor %q outcome %q run %v, want %q success %s", ev.Actor, ev.Outcome, ev.RunID, actor, f.run.ID)
	}
	want := map[string]any{"approval_id": ap.ID.String(), "basis": basis, "host": dcHost, "mode": "audit"}
	for k, v := range want {
		if data[k] != v {
			f.t.Errorf("alert data[%q] = %v, want %v (data %v)", k, data[k], v, data)
		}
	}
	f.wantNoRows("authz.denied", ap)
}

// wantAuthorityRefusal asserts authz.denied for the approval names capability_egress_host, basis and the host,
// and that no alert was written.
func (f *dcFix) wantAuthorityRefusal(ap types.ApprovalRequest, basis string) {
	f.t.Helper()
	_, data := f.authorityData("authz.denied", ap)
	if data["reason"] != "capability_egress_host" || data["basis"] != basis || data["host"] != dcHost {
		f.t.Errorf("authz.denied data = %v, want reason capability_egress_host, basis %s, host %s", data, basis, dcHost)
	}
	f.wantNoRows("approval.authority.alert", ap)
}

func TestApprovalAuthority_UnitAdminWithoutHostAudits(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "audit")
	f := authorityFix(t, types.DeciderUnitAdmins)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("unit admin approve", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantAuthorityAlert(ap, dcAdmin, deciderBasisUnitAdminOf+f.team.ID.String())
}

func TestApprovalAuthority_UnitAdminWithoutHostRefusedUnderEnforce(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	f := authorityFix(t, types.DeciderUnitAdmins)
	ap := f.hold(types.DeciderKindEgressDomain)
	w := f.decide(dcMember(t, dcAdmin), ap, "approve", "")
	f.wantStatus("unit admin approve", w, http.StatusForbidden, "capability_egress_host", ap)
	f.wantAuthorityRefusal(ap, deciderBasisUnitAdminOf+f.team.ID.String())
}

func TestApprovalAuthority_GroupDeciderWithoutHostRefusedUnderEnforce(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	basis := types.DeciderGroupPrefix + dcGroup
	f := authorityFix(t, basis)
	ap := f.hold(types.DeciderKindEgressDomain)
	w := f.decide(dcMember(t, dcLead, dcGroup), ap, "approve", "")
	f.wantStatus("group decider approve", w, http.StatusForbidden, "capability_egress_host", ap)
	f.wantAuthorityRefusal(ap, basis)
}

func TestApprovalAuthority_GroupDeciderWithoutHostAudits(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "audit")
	basis := types.DeciderGroupPrefix + dcGroup
	f := authorityFix(t, basis)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("group decider approve", f.decide(dcMember(t, dcLead, dcGroup), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantAuthorityAlert(ap, dcLead, basis)
}

// TestApprovalAuthority_OnlyUnitAndGroupBasesCapped: the cap reads unit_admin:<unit> and group:<name> only. The
// owner meets the grant in resolveDecider already, and an approval rule, the security tier and the admin token
// are not capped.
func TestApprovalAuthority_OnlyUnitAndGroupBasesCapped(t *testing.T) {
	for basis, want := range map[string]bool{
		"":                                 false,
		types.DeciderOwner:                 false,
		deciderBasisPolicy:                 false,
		deciderBasisSecurity:               false,
		deciderBasisAdminToken:             false,
		types.DeciderGroupPrefix:           false,
		deciderBasisUnitAdminOf + "u-1":    true,
		types.DeciderGroupPrefix + dcGroup: true,
	} {
		if got := deciderAuthorityCapped(basis); got != want {
			t.Errorf("deciderAuthorityCapped(%q) = %v, want %v", basis, got, want)
		}
	}
}

// TestApprovalAuthority_OwnerNotCappedTwice: under a rule naming owner, the owner's grant is checked once, in
// resolveDecider. With the grant the approve goes ahead with no alert; without it the member path refuses with
// one authz.denied that names no decider basis.
func TestApprovalAuthority_OwnerNotCappedTwice(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	f := authorityFix(t, types.DeciderOwner)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("granted owner approve", f.decide(dcMember(t, dcOwner), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantNoRows("approval.authority.alert", ap)
	f.wantNoRows("authz.denied", ap)

	f.st.grants = nil
	ap = f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("ungranted owner approve", f.decide(dcMember(t, dcOwner), ap, "approve", ""), http.StatusForbidden, "capability_egress_host", ap)
	if _, data := f.authorityData("authz.denied", ap); data["basis"] != nil {
		t.Errorf("the owner's refusal names basis %v, want none", data["basis"])
	}
	f.wantNoRows("approval.authority.alert", ap)
}

// TestApprovalAuthority_EmptyHost: an egress approval whose RequestedScope names no host is a gap for a decider
// with no `*` grant: audit approves it and alerts with an empty host, enforce refuses it.
func TestApprovalAuthority_EmptyHost(t *testing.T) {
	for _, mode := range []string{"audit", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(envIdentityNarrowing, mode)
			f := authorityFix(t, types.DeciderUnitAdmins)
			ap := f.hold(types.DeciderKindEgressDomain)
			ap.RequestedScope = json.RawMessage(`{}`)
			f.aps.mu.Lock()
			f.aps.byID[ap.ID] = ap
			f.aps.mu.Unlock()
			w := f.decide(dcMember(t, dcAdmin), ap, "approve", "")
			basis := deciderBasisUnitAdminOf + f.team.ID.String()
			if mode == "enforce" {
				f.wantStatus("enforce empty host", w, http.StatusForbidden, "capability_egress_host", ap)
				if _, data := f.authorityData("authz.denied", ap); data["basis"] != basis || data["host"] != "" {
					t.Errorf("authz.denied data = %v, want basis %s and an empty host", data, basis)
				}
				f.wantNoRows("approval.authority.alert", ap)
				return
			}
			f.wantStatus("audit empty host", w, http.StatusOK, "", ap)
			if _, data := f.authorityData("approval.authority.alert", ap); data["basis"] != basis || data["host"] != "" || data["mode"] != "audit" {
				t.Errorf("alert data = %v, want basis %s, an empty host and mode audit", data, basis)
			}
			f.wantNoRows("authz.denied", ap)
		})
	}
}

func TestApprovalAuthority_DeciderWithHostGrantDecides(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	f := authorityFix(t, types.DeciderUnitAdmins)
	f.st.grants = append(f.st.grants, grant(types.CapabilitySubjectUser, dcAdmin, capEgressHost, dcHost, types.CapabilityAllow))
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("granted unit admin approve", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantNoRows("approval.authority.alert", ap)
	f.wantNoRows("authz.denied", ap)
}

func TestApprovalAuthority_DenyIsNeverCapped(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	f := authorityFix(t, types.DeciderUnitAdmins)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("unit admin deny", f.decide(dcMember(t, dcAdmin), ap, "deny", ""), http.StatusOK, "", ap)
	if got := f.row(ap).State; got != types.ApprovalDenied {
		t.Fatalf("state %s, want DENIED", got)
	}
	f.wantNoRows("approval.authority.alert", ap)
}

func TestApprovalAuthority_SecurityTierNotCapped(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	f := authorityFix(t, types.DeciderUnitAdmins)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("security approve", f.decide(dcSecurity(t, dcSec), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantNoRows("approval.authority.alert", ap)
}

// TestApprovalAuthority_UnenforcedKindChangesNothing: with egress_host unenforced and no grant rows, the cap
// answers true, so a deployment that never wrote an egress_host grant sees no change under enforce.
func TestApprovalAuthority_UnenforcedKindChangesNothing(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "enforce")
	f := authorityFix(t, types.DeciderUnitAdmins)
	f.st.enf, f.st.grants = nil, nil
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("unit admin approve", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantNoRows("approval.authority.alert", ap)
	f.wantNoRows("authz.denied", ap)
}

func TestApprovalAuthority_DefaultModeAudits(t *testing.T) {
	t.Setenv(envIdentityNarrowing, "")
	f := authorityFix(t, types.DeciderUnitAdmins)
	ap := f.hold(types.DeciderKindEgressDomain)
	f.wantStatus("unit admin approve", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
	f.wantAuthorityAlert(ap, dcAdmin, deciderBasisUnitAdminOf+f.team.ID.String())
}

// grantsFaultStore is decidersStore whose capability grant read fails for one subject.
type grantsFaultStore struct {
	*decidersStore
	subject string
}

func (s *grantsFaultStore) ListCapabilityGrantsFor(ctx context.Context, users, groups []string, userType string) ([]types.CapabilityGrant, error) {
	if slices.Contains(users, s.subject) {
		return nil, errors.New("capability grants unreadable")
	}
	return s.decidersStore.ListCapabilityGrantsFor(ctx, users, groups, userType)
}

// TestApprovalAuthority_AuditReadErrorWarnsAndDecides: a decider's capability read that fails is logged under
// audit and the decision goes ahead with no alert row; under enforce the same fault answers 500 and the
// approval stays PENDING.
func TestApprovalAuthority_AuditReadErrorWarnsAndDecides(t *testing.T) {
	for _, mode := range []string{"audit", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(envIdentityNarrowing, mode)
			f := authorityFix(t, types.DeciderUnitAdmins)
			cfg := baseTestConfig(f.h, &grantsFaultStore{decidersStore: f.st, subject: dcAdmin})
			cfg.OIDC = &oidc.Authenticator{}
			cfg.Approvals = f.aps
			f.srv = New(cfg)
			var logged bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			ap := f.hold(types.DeciderKindEgressDomain)
			w := f.decide(dcMember(t, dcAdmin), ap, "approve", "")
			f.wantNoRows("approval.authority.alert", ap)
			if mode == "enforce" {
				f.wantStatus("enforce read error", w, http.StatusInternalServerError, "", ap)
				return
			}
			f.wantStatus("audit read error", w, http.StatusOK, "", ap)
			if got := f.row(ap).State; got != types.ApprovalApproved {
				t.Fatalf("state %s, want APPROVED", got)
			}
			var warns []string
			for _, line := range strings.Split(logged.String(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "decider authority not checked") {
					warns = append(warns, line)
				}
			}
			if len(warns) != 1 || !strings.Contains(warns[0], ap.ID.String()) {
				t.Errorf("WARN lines naming the approval = %q, want one (log %s)", warns, logged.String())
			}
		})
	}
}
