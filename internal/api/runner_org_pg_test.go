// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Organization-owned runners and the trust class against a real Postgres (0.9.3, migration 0180): the
// org mint is the security tier's and the person mint refuses an owner kind; the org claim needs a human
// security operator, different from a human minter under four-eyes, and each claim refuses the other
// kind; the trust class changes only on PUT /runners/{id}/trust, by a security operator, audited, and
// held for a second human under four-eyes. Guarded by WARDYN_TEST_PG: skipped when unset.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newOrgRunnerEnv(t *testing.T, secondHuman bool) *poolEnv {
	t.Helper()
	e := newPoolEnv(t, secondHuman)
	e.enableRunners()
	return e
}

// as calls the server as c, or as the admin token when c is nil.
func (e *poolEnv) as(c *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	if c == nil {
		return e.admin(method, path, body)
	}
	return e.call(c, method, path, body)
}

// mintOrg mints an org token as c (nil: the admin token) and returns the answer.
func (e *poolEnv) mintOrg(c *http.Cookie) types.RunnerRegistrationToken {
	e.t.Helper()
	body := `{"name":"cluster-a"}`
	w := e.as(c, http.MethodPost, "/api/v1/runners/org-tokens", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("org mint = %d %s", w.Code, w.Body)
	}
	var tok types.RunnerRegistrationToken
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// registerWith redeems tok as a runner would and returns the new runner's id.
func (e *poolEnv) registerWith(tok string) uuid.UUID {
	e.t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	body, _ := json.Marshal(types.RunnerRegisterRequest{Token: tok, PublicKey: pub, Name: "cluster-a", Limits: roomyRunnerLimits()})
	w := do(e.t, e.srv, http.MethodPost, "/api/v1/runners/register", "", string(body))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	var reg types.RunnerRegistration
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
		e.t.Fatal(err)
	}
	return reg.RunnerID
}

func (e *poolEnv) runnerRow(id uuid.UUID) types.Runner {
	e.t.Helper()
	row, err := e.pg.GetRunner(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return row
}

func (e *poolEnv) claimOrg(c *http.Cookie, id uuid.UUID) (int, string) {
	e.t.Helper()
	body := `{"fingerprint":"` + e.runnerRow(id).KeyFingerprint + `"}`
	path := "/api/v1/runners/" + id.String() + "/claim-org"
	w := e.as(c, http.MethodPost, path, body)
	return w.Code, w.Body.String()
}

// auditData is the data of each audit row of action on target, oldest first.
func (e *poolEnv) auditData(action, target string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action != action || ev.Target != target {
			continue
		}
		var data map[string]any
		_ = json.Unmarshal(ev.Data, &data)
		out = append(out, data)
	}
	return out
}

func TestPG_OrgRunnerTokenDoors(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	if w := e.call(e.user, http.MethodPost, "/api/v1/runners/org-tokens", `{"name":"cluster-a"}`); w.Code != http.StatusForbidden {
		t.Fatalf("a member's org mint = %d %s, want 403", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, "/api/v1/runners/org-tokens", `{"name":" "}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an org mint with no name = %d %s, want 422", w.Code, w.Body)
	}
	human := e.mintOrg(e.alice)
	if human.OwnerKind != types.RunnerOwnerOrg || human.Owner != "" || human.MintedByKind != types.RunnerMintedByHuman || human.BreakGlass || !strings.HasPrefix(human.Token, "wdr_") ||
		!human.ExpiresAt.Equal(human.CreatedAt.Add(72*time.Hour)) {
		t.Fatalf("a security admin's org token = %+v", human)
	}
	if data := e.auditData(placement.ActionRunnerTokenCreate, human.ID.String()); len(data) != 1 || data[0]["owner_kind"] != "org" ||
		data[0]["minted_by_kind"] != "human" || data[0]["name"] != "cluster-a" {
		t.Fatalf("runner.token.create rows = %v", data)
	}
	if tok := e.mintOrg(nil); tok.MintedByKind != types.RunnerMintedByAdminToken || tok.BreakGlass {
		t.Fatalf("the admin token's org token with four-eyes off = %+v, want admin_token without break-glass", tok)
	}
	w := e.call(e.super, http.MethodPost, "/api/v1/runners/tokens", `{"owner":"sub-person","owner_kind":"org"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("the person mint naming an owner kind = %d %s, want 400", w.Code, w.Body)
	}
	id := e.registerWith(human.Token)
	if row := e.runnerRow(id); row.OwnerKind != types.RunnerOwnerOrg || row.Owner != "" || row.TrustClass != types.RunnerTrustOrgCluster ||
		row.State != types.RunnerUnclaimed || row.RegistrationTokenID == nil || *row.RegistrationTokenID != human.ID {
		t.Fatalf("the org runner registered as %+v", row)
	}
}

func TestPG_OrgRunnerClaimFourEyes(t *testing.T) {
	e := newOrgRunnerEnv(t, true)
	id := e.registerWith(e.mintOrg(e.alice).Token)
	if w := e.call(e.alice, http.MethodPost, "/api/v1/me/runners/"+id.String()+"/claim", `{"fingerprint":"`+e.runnerRow(id).KeyFingerprint+`"}`); w.Code != http.StatusForbidden {
		t.Fatalf("the person claim of an org runner = %d %s, want 403", w.Code, w.Body)
	}
	for name, c := range map[string]*http.Cookie{"a member": e.user, "the minter": e.alice, "the minter's email twin": e.twin, "the admin token": nil} {
		if code, body := e.claimOrg(c, id); code != http.StatusForbidden {
			t.Fatalf("%s claimed the org runner: %d %s", name, code, body)
		}
	}
	if code, body := e.claimOrg(e.alice, id); !strings.Contains(body, "second_human_required") {
		t.Fatalf("the minter's claim was refused for another reason: %d %s", code, body)
	}
	if row := e.runnerRow(id); row.State != types.RunnerUnclaimed {
		t.Fatalf("a refused claim changed the runner: %+v", row)
	}
	if code, body := e.claimOrg(e.bob, id); code != http.StatusOK {
		t.Fatalf("a second security admin's claim = %d %s", code, body)
	}
	if row := e.runnerRow(id); row.State != types.RunnerClaimed || row.TrustClass != types.RunnerTrustOrgCluster || row.TrustSetBy != govSubBob {
		t.Fatalf("the claimed org runner = %+v", row)
	}
	if data := e.auditData(placement.ActionRunnerClaim, id.String()); len(data) != 1 || data[0]["four_eyes"] != true || data[0]["break_glass"] != false || data[0]["owner_kind"] != "org" {
		t.Fatalf("runner.claim rows = %v", data)
	}
	person := e.claimedRunner("sub-person")
	if code, body := e.claimOrg(e.bob, person); code != http.StatusForbidden {
		t.Fatalf("the org claim of a person's runner = %d %s, want 403", code, body)
	}
}

func TestPG_OrgRunnerBreakGlassMint(t *testing.T) {
	e := newOrgRunnerEnv(t, true)
	tok := e.mintOrg(nil)
	if tok.MintedByKind != types.RunnerMintedByAdminToken || !tok.BreakGlass {
		t.Fatalf("the admin token's org token under four-eyes = %+v, want break-glass", tok)
	}
	if rows := e.auditData("governance.change.bypass", tok.ID.String()); len(rows) != 1 || rows[0]["target_kind"] != govTargetRunnerOrgToken {
		t.Fatalf("governance.change.bypass rows = %v", rows)
	}
	id := e.registerWith(tok.Token)
	if code, body := e.claimOrg(e.alice, id); code != http.StatusOK {
		t.Fatalf("a security admin's claim of a break-glass token = %d %s", code, body)
	}
	if data := e.auditData(placement.ActionRunnerClaim, id.String()); len(data) != 1 || data[0]["four_eyes"] != false || data[0]["break_glass"] != true {
		t.Fatalf("runner.claim rows = %v", data)
	}
	if w := e.call(e.alice, http.MethodDelete, "/api/v1/runners/"+id.String(), ""); w.Code != http.StatusOK {
		t.Fatalf("revoking the org runner = %d %s", w.Code, w.Body)
	}
}

func (e *poolEnv) putTrust(c *http.Cookie, id uuid.UUID, body string) (int, string) {
	e.t.Helper()
	path := "/api/v1/runners/" + id.String() + "/trust"
	w := e.as(c, http.MethodPut, path, body)
	return w.Code, w.Body.String()
}

func TestPG_RunnerTrustWrite(t *testing.T) {
	e := newOrgRunnerEnv(t, false)
	id := e.claimedRunner("sub-person")
	md := `{"trust_class":"managed_device","reason":"enrolled in MDM"}`
	if code, body := e.putTrust(e.user, id, md); code != http.StatusForbidden {
		t.Fatalf("a member's trust write = %d %s, want 403", code, body)
	}
	if code, body := e.putTrust(e.person, id, md); code != http.StatusForbidden {
		t.Fatalf("the runner's owner's trust write = %d %s, want 403", code, body)
	}
	for body, want := range map[string]int{
		`{"trust_class":"org_cluster","reason":"x"}`:    http.StatusUnprocessableEntity,
		`{"trust_class":"trusted","reason":"x"}`:        http.StatusBadRequest,
		`{"trust_class":"managed_device"}`:              http.StatusBadRequest,
		`{"trust_class":"managed_device","reason":" "}`: http.StatusBadRequest,
	} {
		if code, got := e.putTrust(e.alice, id, body); code != want || !strings.Contains(got, "runner_trust_invalid") {
			t.Fatalf("PUT trust %s = %d %s, want %d runner_trust_invalid", body, code, got, want)
		}
	}
	if code, _ := e.putTrust(e.alice, uuid.New(), md); code != http.StatusNotFound {
		t.Fatalf("PUT trust on no runner = %d, want 404", code)
	}
	if row := e.runnerRow(id); row.TrustClass != types.RunnerTrustPersonal {
		t.Fatalf("a refused write changed the class to %q", row.TrustClass)
	}
	code, body := e.putTrust(e.alice, id, md)
	var view types.RunnerView
	if code != http.StatusOK || json.Unmarshal([]byte(body), &view) != nil || view.TrustClass != types.RunnerTrustManagedDevice || view.TrustSetBy != govSubAlice || view.OwnerKind != types.RunnerOwnerPerson {
		t.Fatalf("a security admin's trust write = %d %s", code, body)
	}
	if data := e.auditData(placement.ActionRunnerTrustWrite, id.String()); len(data) != 1 || data[0]["before"] != "personal" || data[0]["after"] != "managed_device" || data[0]["reason"] != "enrolled in MDM" {
		t.Fatalf("runner.trust.write rows = %v", data)
	}
	org := e.registerWith(e.mintOrg(e.alice).Token)
	if code, body := e.putTrust(e.alice, org, md); code != http.StatusConflict {
		t.Fatalf("PUT trust on an unclaimed runner = %d %s, want 409", code, body)
	}
	if code, body := e.claimOrg(e.alice, org); code != http.StatusOK {
		t.Fatalf("org claim = %d %s", code, body)
	}
	if code, body := e.putTrust(e.alice, org, `{"trust_class":"personal","reason":"x"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("personal on an org runner = %d %s, want 422", code, body)
	}
}

func TestPG_RunnerTrustWriteFourEyes(t *testing.T) {
	e := newOrgRunnerEnv(t, true)
	id := e.claimedRunner("sub-person")
	w := e.call(e.alice, http.MethodPut, "/api/v1/runners/"+id.String()+"/trust", `{"trust_class":"managed_device","reason":"enrolled in MDM"}`)
	ch := e.pending(w)
	if ch.TargetKind != govKindRunnerTrust || e.runnerRow(id).TrustClass != types.RunnerTrustPersonal {
		t.Fatalf("a held trust change = %+v, or it was applied before approval", ch)
	}
	if w := e.call(e.alice, http.MethodPost, "/api/v1/governance/changes/"+ch.ID.String()+"/approve", ""); w.Code != http.StatusForbidden {
		t.Fatalf("the proposer approved their own trust change: %d %s", w.Code, w.Body)
	}
	e.approve(ch.ID)
	if row := e.runnerRow(id); row.TrustClass != types.RunnerTrustManagedDevice || row.TrustSetBy != govSubAlice {
		t.Fatalf("the approved trust change left %+v", row)
	}
	if data := e.auditData(placement.ActionRunnerTrustWrite, id.String()); len(data) != 1 || data[0]["after"] != "managed_device" {
		t.Fatalf("runner.trust.write rows = %v", data)
	}
	if code, body := e.putTrust(nil, id, `{"trust_class":"personal","reason":"device retired"}`); code != http.StatusOK {
		t.Fatalf("the admin token's trust write under four-eyes = %d %s", code, body)
	}
	if rows := e.auditData("governance.change.bypass", id.String()); len(rows) != 1 || rows[0]["target_kind"] != govKindRunnerTrust {
		t.Fatalf("governance.change.bypass rows = %v", rows)
	}
}
