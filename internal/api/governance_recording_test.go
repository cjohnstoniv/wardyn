// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const governanceRecordingPath = "/api/v1/governance/recording"

func TestGovernanceRecordingIsASecurityOperatorWrite(t *testing.T) {
	srv, fake, h := egressBaselineServer(t, types.SiteConfig{
		Egress:    &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}},
		Recording: &types.SiteRecording{Level: types.RecordingOff},
	})
	const body = `{"level":"screen_actions"}`
	member := ssoSession(t, "sub-m", "m@corp.example", oidc.RoleUser)
	if w := doSSO(t, srv, http.MethodPut, governanceRecordingPath, member, body); w.Code != http.StatusForbidden {
		t.Fatalf("member PUT = %d %s, want 403", w.Code, w.Body)
	}
	if w := doSSO(t, srv, http.MethodGet, governanceRecordingPath, member, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member GET = %d, want 403", w.Code)
	}
	if fake.putSeen != nil || len(govCovAudits(h, recordingWriteAction)) != 0 {
		t.Fatal("a refused member write reached the store or the audit log")
	}

	secAdmin := ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin)
	var got types.SiteRecording
	if w := doSSO(t, srv, http.MethodGet, governanceRecordingPath, secAdmin, ""); w.Code != http.StatusOK ||
		json.Unmarshal(w.Body.Bytes(), &got) != nil || got != (types.SiteRecording{Level: types.RecordingOff}) {
		t.Fatalf("GET = %d %s, want the stored block (the security tier cannot read GET /site-config)", w.Code, w.Body)
	}
	if w := doSSO(t, srv, http.MethodPut, governanceRecordingPath, secAdmin, body); w.Code != http.StatusOK {
		t.Fatalf("security admin PUT = %d %s, want 200", w.Code, w.Body)
	}
	if fake.cfg.Recording == nil || *fake.cfg.Recording != (types.SiteRecording{Level: types.RecordingScreenActions}) {
		t.Errorf("stored recording = %+v, want screen_actions, unlocked", fake.cfg.Recording)
	}
	if fake.cfg.Egress == nil || !slices.Equal(fake.cfg.Egress.BaselineHosts, []string{"llm.corp.example"}) {
		t.Errorf("stored egress = %+v, want it untouched: the route writes org.Recording only", fake.cfg.Egress)
	}
	rows := govCovAudits(h, recordingWriteAction)
	if len(rows) != 1 || rows[0].Actor != "sub-s" || rows[0].Target != govRecordingKey {
		t.Fatalf("audit rows = %+v, want one by the writer", rows)
	}
	d := govCovAuditData(t, rows[0])
	if d["level"] != "screen_actions" || d["locked"] != false || d["was_level"] != "off" || d["was_locked"] != false {
		t.Errorf("audit data = %v, want the level and lock after and before", d)
	}
	if n := len(govCovAudits(h, "site_config.write")); n != 0 {
		t.Errorf("%d site_config.write rows: the recording write is audited on its own action", n)
	}

	// An all-default block is stored as none, as PUT /site-config stores it.
	if w := doSSO(t, srv, http.MethodPut, governanceRecordingPath, secAdmin, `{}`); w.Code != http.StatusOK || fake.cfg.Recording != nil {
		t.Errorf("clear = %d, recording %+v, want 200 and no block", w.Code, fake.cfg.Recording)
	}
}

func TestGovernanceRecordingRefusesAnyOtherField(t *testing.T) {
	srv, fake, h := egressBaselineServer(t, types.SiteConfig{})
	sec := ssoSession(t, "sub-s", "s@corp.example", oidc.RoleSecurityAdmin)
	for name, body := range map[string]string{
		"another site-config field": `{"level":"terminal","egress":{"baseline_hosts":["x.corp.example"]}}`,
		"a whole site config":       `{"recording":{"level":"terminal"}}`,
	} {
		w := doSSO(t, srv, http.MethodPut, governanceRecordingPath, sec, body)
		wantReason(t, w, http.StatusBadRequest, reasonInvalidRequestBody)
		if fake.putSeen != nil {
			t.Fatalf("%s: the refused body reached the store", name)
		}
	}
	wantReason(t, doSSO(t, srv, http.MethodPut, governanceRecordingPath, sec, `{"level":"keystrokes"}`), http.StatusBadRequest, reasonRecordingInvalid)
	// A locked minimum above off with no recording store is PUT /site-config's own 422.
	wantReason(t, doSSO(t, srv, http.MethodPut, governanceRecordingPath, sec, `{"level":"terminal","locked":true}`),
		placement.ReasonRecordingStoreOff.Status(), string(placement.ReasonRecordingStoreOff))
	if fake.putSeen != nil || len(govCovAudits(h, recordingWriteAction)) != 0 {
		t.Fatal("a refused write reached the store or the audit log")
	}
}

func TestGovernanceRecordingReplacementByAHumanIsHeld(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	st := &govCovStore{proposeSaved: types.GovernanceChange{ID: govCovChangeID}}
	srv, _ := govCovServer(t, st)
	sec := govCovSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)

	w := doSSO(t, srv, http.MethodPut, governanceRecordingPath, sec, `{"level":"off","locked":true}`)
	ch := govCovHoldAnswer(t, w, st)
	if ch.TargetKind != govKindRecording || ch.Op != "replace" || ch.TargetKey != govRecordingKey {
		t.Fatalf("stored change = %+v", ch)
	}
	var payload types.SiteRecording
	if err := json.Unmarshal(ch.Payload, &payload); err != nil || payload != (types.SiteRecording{Level: types.RecordingOff, Locked: true}) {
		t.Fatalf("payload = %s (%v)", ch.Payload, err)
	}
	// A refused value is the direct 400, not a held change.
	st.proposed = nil
	if w := doSSO(t, srv, http.MethodPut, governanceRecordingPath, sec, `{"level":"keystrokes"}`); w.Code != http.StatusBadRequest || len(st.proposed) != 0 {
		t.Errorf("bad level = %d with %d held, want 400 and none", w.Code, len(st.proposed))
	}
}

// TestPutSiteConfigNeverChangesTheRecordingLevel: PUT /site-config passes the stored recording block back
// (a body silent about it, or a round trip of GET) and refuses any change, writing nothing: the block is
// PUT /governance/recording's, as the egress baseline is PUT /governance/egress-baseline's.
func TestPutSiteConfigNeverChangesTheRecordingLevel(t *testing.T) {
	stored := types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingScreenActions}}
	srv, fake, _ := egressBaselineServer(t, stored)
	for _, body := range []string{`{}`, `{"recording":{"level":"screen_actions"}}`} {
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s, want 200", body, w.Code, w.Body)
		}
		if fake.cfg.Recording == nil || *fake.cfg.Recording != *stored.Recording {
			t.Fatalf("after PUT %s the stored recording = %+v, want it as it was", body, fake.cfg.Recording)
		}
	}
	for _, body := range []string{`{"recording":{"level":"off"}}`, `{"recording":{"level":"terminal"}}`, `{"recording":{}}`} {
		fake.putSeen = nil
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonSiteConfigRecordingViaOwnRoute ||
			!strings.Contains(w.Body.String(), "PUT /governance/recording") {
			t.Errorf("PUT %s = %d %s, want 400 %s naming PUT /governance/recording", body, w.Code, w.Body, reasonSiteConfigRecordingViaOwnRoute)
		}
		if fake.putSeen != nil || fake.cfg.Recording == nil || *fake.cfg.Recording != *stored.Recording {
			t.Errorf("refused PUT %s wrote %+v", body, fake.putSeen)
		}
	}
}

// TestPutSiteConfigRoundTripsALockedLevelWithNoStore: a locked minimum stored while the organisation
// kept recordings still round-trips through PUT /site-config after the store is gone, since this door
// cannot change the block; PUT /governance/recording is the one that refuses it.
func TestPutSiteConfigRoundTripsALockedLevelWithNoStore(t *testing.T) {
	stored := types.SiteConfig{Recording: &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}}
	srv, fake, _ := egressBaselineServer(t, stored)
	srv.cfg.RecordingStore = nil
	// The echo carries an unrelated edit, as a console save does.
	body := `{"recording":{"level":"terminal","locked":true},"sign_in_help_text":"Ask the platform team."}`
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("PUT %s with no recording store = %d %s, want 200", body, w.Code, w.Body)
	}
	if fake.cfg.Recording == nil || *fake.cfg.Recording != *stored.Recording {
		t.Fatalf("after the round trip the stored recording = %+v, want it as it was", fake.cfg.Recording)
	}
	if fake.cfg.SignInHelpText != "Ask the platform team." {
		t.Fatalf("the edit beside the echoed recording block was not saved: %q", fake.cfg.SignInHelpText)
	}
}
