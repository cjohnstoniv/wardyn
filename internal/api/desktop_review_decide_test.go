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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Who decides a reviewed download: with no rule, the security tier; under a unit's file_download rule, its
// deciders too. Nobody approves a download of their own: the run's own people and the person who asked never
// decide, the security tier included. A decision is always once. On the fixture of deciders_resolve_test.go.

// holdDownload seeds a PENDING file_download row on the run, asked by requester.
func (f *dcFix) holdDownload(requester string) types.ApprovalRequest {
	f.t.Helper()
	raw, _ := json.Marshal(types.FileDownloadScope{Name: "report.csv", Bytes: 17,
		SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", RequestedBy: requester})
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalFileDownload, RequestedScope: raw,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	f.aps.mu.Lock()
	f.aps.byID[ap.ID] = ap
	f.aps.mu.Unlock()
	return ap
}

// TestDesktopReviewOwnerCannotDecide: the run's owner never decides its download: 404 as for any kind a member
// does not decide, with or without a rule.
func TestDesktopReviewOwnerCannotDecide(t *testing.T) {
	f := newDCFix(t)
	ap := f.holdDownload(dcOwner)
	f.wantStatus("owner, no rule", f.decide(dcMember(t, dcOwner), ap, "approve", ""), http.StatusNotFound, "", ap)
	f.st.setRules(f.root.ID, types.DeciderRule{Kinds: []string{types.DeciderKindFileDownload}, Who: []string{types.DeciderUnitAdmins}, SecondHuman: true})
	f.wantStatus("owner under a rule", f.decide(dcMember(t, dcOwner), ap, "approve", ""), http.StatusNotFound, "", ap)
	if f.mayDecideAs(dcCtx(dcOwner, oidc.RoleUser, nil, false), ap) {
		t.Error("mayDecide = true for the run's owner")
	}
}

// TestDesktopReviewSecurityDecidesOnce: with no rule the security tier decides; the decision is stored once, and
// a wider scope is refused.
func TestDesktopReviewSecurityDecidesOnce(t *testing.T) {
	f := newDCFix(t)
	ap := f.holdDownload(dcOwner)
	f.wantStatus("scope run", f.decide(dcSecurity(t, dcSec), ap, "approve", `{"decision_scope":"run"}`), http.StatusBadRequest, reasonDecisionScopeInvalidForKind, ap)
	f.wantStatus("security", f.decide(dcSecurity(t, dcSec), ap, "approve", ""), http.StatusOK, "", ap)
	if got := f.row(ap); got.State != types.ApprovalApproved || got.DecisionScope != types.ScopeOnce {
		t.Fatalf("decided row = %s %q, want APPROVED once", got.State, got.DecisionScope)
	}
	if !f.mayDecideAs(dcCtx(dcSec2, oidc.RoleSecurityAdmin, nil, false), f.holdDownload(dcOwner)) {
		t.Error("mayDecide = false for a security admin")
	}
}

// TestDesktopReviewSecurityOwnerRefused: a security admin who created the run, or who asked for the download,
// does not decide it; another security admin does.
func TestDesktopReviewSecurityOwnerRefused(t *testing.T) {
	for name, c := range map[string]struct {
		creator, requester string
	}{
		"created the run": {dcSec, dcOwner},
		"asked":           {dcOwner, dcSec},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDCFix(t, createdBy(c.creator))
			ap := f.holdDownload(c.requester)
			if f.mayDecideAs(dcCtx(dcSec, oidc.RoleSecurityAdmin, nil, false), ap) {
				t.Error("mayDecide = true for the security admin's own download")
			}
			f.wantStatus("own download", f.decide(dcSecurity(t, dcSec), ap, "approve", ""), http.StatusForbidden, "second_human_required", ap)
			f.wantStatus("another security admin", f.decide(dcSecurity(t, dcSec2), ap, "approve", ""), http.StatusOK, "", ap)
		})
	}
}

// TestDesktopReviewAdminTokenBypassAudited: the admin token decides as the operator break-glass, and the bypass
// of the download's second-person rule is audited.
func TestDesktopReviewAdminTokenBypassAudited(t *testing.T) {
	f := newDCFix(t)
	ap := f.holdDownload(dcOwner)
	f.wantStatus("admin token", f.decide(nil, ap, "approve", ""), http.StatusOK, "", ap)
	bypass := f.actionRows("approval.second_human.bypass", ap.ID.String())
	if len(bypass) != 1 || bypass[0].Outcome != "success" {
		t.Fatalf("bypass rows = %+v, want one success row", bypass)
	}
	if sw, _ := auditData(t, bypass[0])["switch"].(string); sw != fileDownloadSecondHumanSwitch {
		t.Errorf("bypass switch = %q, want %q", sw, fileDownloadSecondHumanSwitch)
	}
}

// TestDesktopReviewDeciderRuleAllows: under a unit's file_download rule its group decides, recorded with the
// group as the basis; a group member who created the run or asked does not; without the rule nobody in the
// group decides.
func TestDesktopReviewDeciderRuleAllows(t *testing.T) {
	f := newDCFix(t, createdBy(dcLead2))
	ap := f.holdDownload(dcOwner)
	f.wantStatus("group, no rule", f.decide(dcMember(t, dcLead, dcGroup), ap, "approve", ""), http.StatusNotFound, "", ap)
	f.st.setRules(f.root.ID, types.DeciderRule{Kinds: []string{types.DeciderKindFileDownload}, Who: []string{types.DeciderGroupPrefix + dcGroup}, SecondHuman: true})
	if !f.mayDecideAs(dcCtx(dcLead, oidc.RoleUser, []string{dcGroup}, false), ap) {
		t.Error("mayDecide = false for the rule's group")
	}
	f.wantStatus("creator in the group", f.decide(dcMember(t, dcLead2, dcGroup), ap, "approve", ""), http.StatusNotFound, "", ap)
	asked := f.holdDownload(dcLead)
	if w := f.decide(dcMember(t, dcLead, dcGroup), asked, "approve", ""); w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "second") {
		t.Fatalf("the group member who asked decided it: %d %s", w.Code, w.Body)
	}
	f.wantStatus("the group", f.decide(dcMember(t, dcLead, dcGroup), ap, "approve", ""), http.StatusOK, "", ap)
	rows := f.actionRows("approval.decide", ap.ID.String())
	if len(rows) != 1 {
		t.Fatalf("approval.decide rows = %d", len(rows))
	}
	data := auditData(t, rows[0])
	rule, _ := data["rule"].(map[string]any)
	if data["basis"] != types.DeciderGroupPrefix+dcGroup || rule["kind"] != types.DeciderKindFileDownload {
		t.Errorf("approval.decide data = %s", rows[0].Data)
	}
	if got := f.row(ap).DecisionScope; got != types.ScopeOnce {
		t.Errorf("decision scope = %q, want once", got)
	}
}
