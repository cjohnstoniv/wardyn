// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func govCovGrant() types.CapabilityGrant {
	return types.CapabilityGrant{
		ID: govCovGrantID, SubjectType: types.CapabilitySubjectUser, Subject: "u@corp.example",
		Capability: capEgressHost, Value: "pypi.org", Effect: types.CapabilityAllow,
	}
}

func govCovGrantPayload(t *testing.T, g types.CapabilityGrant, withID bool) json.RawMessage {
	t.Helper()
	p := grantChangePayload{SubjectType: g.SubjectType, Subject: g.Subject, Capability: g.Capability, Value: g.Value, Effect: g.Effect}
	if withID {
		p.ID = g.ID
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGovCovGrantHelpers(t *testing.T) {
	g := govCovGrant()
	var parts []string
	if err := json.Unmarshal([]byte(grantTargetKey(g)), &parts); err != nil || !reflect.DeepEqual(parts, []string{"user", "u@corp.example", capEgressHost, "pypi.org"}) {
		t.Errorf("target key %s decodes to %v (%v)", grantTargetKey(g), parts, err)
	}
	a := types.CapabilityGrant{SubjectType: "user", Subject: "a,b", Capability: "c", Value: "d"}
	b := types.CapabilityGrant{SubjectType: "user", Subject: "a", Capability: "b,c", Value: "d"}
	if grantTargetKey(a) == grantTargetKey(b) {
		t.Error("two grants whose fields differ only in where a separator falls share a target key")
	}
	if v := newGrantDiffView(g); v.SubjectType != g.SubjectType || v.Subject != g.Subject || v.Capability != g.Capability || v.Value != g.Value || v.Effect != g.Effect {
		t.Errorf("view = %+v", v)
	}
	if got := grantState(nil).(map[string]any); got["grant"] != nil || len(got) != 1 {
		t.Errorf("state of no grant = %v", got)
	}
	if got := grantState(&g).(map[string]any); got["grant"].(types.CapabilityGrant).ID != g.ID {
		t.Errorf("state of a grant = %v", got)
	}
	if computeETag(grantState(nil)) == computeETag(grantState(&g)) {
		t.Error("a grant appearing must change the base hash")
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	q := &govCovQuerier{}
	if cur, err := grantAtKeyQ(r, q, g, true); cur != nil || err != nil {
		t.Errorf("no row = %v, %v; want nil, nil", cur, err)
	}
	if len(q.seen) != 1 || !strings.HasSuffix(strings.TrimSpace(q.seen[0]), "FOR UPDATE") {
		t.Errorf("statements = %v, want the locking read", q.seen)
	}
	boom := errors.New("pg: down")
	if _, err := grantAtKeyQ(r, &govCovQuerier{rowErr: func(string) error { return boom }}, g, false); !errors.Is(err, boom) {
		t.Errorf("a failing read = %v", err)
	}
}

func TestGovCovDecodeHeldPayloadIsStrict(t *testing.T) {
	var p grantChangePayload
	if err := decodeHeldPayload(json.RawMessage(`{"subject":"x"}`), &p); err != nil || p.Subject != "x" {
		t.Errorf("a known payload = %v, %+v", err, p)
	}
	if err := decodeHeldPayload(json.RawMessage(`{"subject":"x","extra":1}`), &p); err == nil {
		t.Error("an unknown field must be refused, as a request body's is")
	}
}

func TestGovCovHoldGrants(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	saved := types.GovernanceChange{ID: govCovChangeID}
	upsert := `{"subject_type":"user","subject":"u@corp.example","capability":"egress_host","value":"pypi.org","effect":"allow"}`
	g := govCovGrant()

	t.Run("an upsert of a new grant is held keyed on its natural key", func(t *testing.T) {
		q := &govCovQuerier{}
		st := &govCovStore{proposeSaved: saved, dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPost, "/api/v1/permissions/grants", super, upsert)
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindGrant || ch.Op != "upsert" || ch.TargetKey != grantTargetKey(g) || ch.BaseHash != computeETag(grantState(nil)) {
			t.Errorf("stored change = %+v", ch)
		}
		if string(ch.Payload) != string(govCovGrantPayload(t, g, false)) {
			t.Errorf("payload = %s", ch.Payload)
		}
		if d := govCovDiff(t, ch); d.Before != nil || len(d.Changed) != 5 {
			t.Errorf("diff = %s, want an after view with every field changed", ch.Diff)
		}
		if len(q.writes()) != 0 || len(q.seen) != 1 {
			t.Errorf("the dry run issued %v, want one read and no write", q.seen)
		}
	})

	t.Run("a failing read is a 500 and holds nothing", func(t *testing.T) {
		st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return errors.New("pg: down") }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPost, "/api/v1/permissions/grants", super, upsert)
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 {
			t.Errorf("= %d, proposals %d", w.Code, len(st.proposed))
		}
	})

	deletePath := "/api/v1/permissions/grants/" + govCovGrantID.String()
	t.Run("a delete of an unknown grant is the direct delete's 404", func(t *testing.T) {
		q := &govCovQuerier{}
		st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, deletePath, super, "")
		if w.Code != http.StatusNotFound || errorReason(w) != reasonCapabilityGrantNotFound || len(st.proposed) != 0 {
			t.Errorf("= %d %q, proposals %d", w.Code, errorReason(w), len(st.proposed))
		}
	})

	t.Run("a delete whose read fails is a 500", func(t *testing.T) {
		st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return errors.New("pg: down") }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, deletePath, super, "")
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 {
			t.Errorf("= %d, proposals %d", w.Code, len(st.proposed))
		}
	})
}

func TestGovCovApplyGrantChange(t *testing.T) {
	boom := errors.New("pg: down")
	g := govCovGrant()
	r := govCovHumanReq(http.MethodPost, "/x", "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin)
	held := func(op string, payload json.RawMessage) types.GovernanceChange {
		return types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindGrant, Op: op, Payload: payload, ProposedBy: "sub-alice", BaseHash: computeETag(grantState(nil))}
	}
	asError := func(t *testing.T, err error) {
		t.Helper()
		var ref *profileWriteError
		if err == nil || errors.As(err, &ref) {
			t.Errorf("error = %v, want a plain error", err)
		}
	}

	t.Run("a payload with an unknown field is an error before any statement", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		_, err := applyGrantChange(s, r, q, held("upsert", json.RawMessage(`{"subject":"x","extra":1}`)))
		if err == nil || !strings.Contains(err.Error(), "payload") || len(q.seen) != 0 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("a failed target lock stops the apply", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: govCovExecFailingOn(1, boom)}
		if _, err := applyGrantChange(s, r, q, held("upsert", govCovGrantPayload(t, g, false))); !errors.Is(err, boom) || len(q.seen) != 1 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("a failed read of the grant writes nothing", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{rowErr: func(string) error { return boom }}
		if _, err := applyGrantChange(s, r, q, held("upsert", govCovGrantPayload(t, g, false))); !errors.Is(err, boom) || len(q.writes()) != 0 {
			t.Errorf("error = %v, writes %v", err, q.writes())
		}
	})

	t.Run("a grant that changed since the proposal is stale", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		ch := held("delete", govCovGrantPayload(t, g, true))
		ch.BaseHash = `"old"`
		if _, err := applyGrantChange(s, r, q, ch); !errors.Is(err, store.ErrGovernanceChangeStale) || len(q.writes()) != 0 {
			t.Errorf("error = %v, writes %v", err, q.writes())
		}
	})

	t.Run("a delete removes the held id", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 1")}
		got, err := applyGrantChange(s, r, q, held("delete", govCovGrantPayload(t, g, true)))
		if err != nil || got.action != "capability.grant.delete" || got.target != govCovGrantID.String() {
			t.Fatalf("applied = %+v, %v", got, err)
		}
		if w := q.writes(); len(w) != 1 || !strings.Contains(w[0], "DELETE FROM capability_grants") {
			t.Errorf("writes = %v", w)
		}
	})

	t.Run("a delete whose row is gone is the direct delete's 404", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 0")}
		_, err := applyGrantChange(s, r, q, held("delete", govCovGrantPayload(t, g, true)))
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusNotFound || ref.reason != reasonCapabilityGrantNotFound {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("a failing delete statement is a plain error", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{execFn: func(sql string) (pgconn.CommandTag, error) {
			if strings.HasPrefix(strings.TrimSpace(sql), "DELETE") {
				return pgconn.CommandTag{}, boom
			}
			return pgconn.NewCommandTag("SELECT 1"), nil
		}}
		_, err := applyGrantChange(s, r, q, held("delete", govCovGrantPayload(t, g, true)))
		asError(t, err)
		if !errors.Is(err, boom) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("an upsert that no longer validates is a 400 and writes nothing", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		bad := g
		bad.Capability = "not_a_kind"
		_, err := applyGrantChange(s, r, q, held("upsert", govCovGrantPayload(t, bad, false)))
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusBadRequest || ref.reason != reasonCapabilityGrantInvalid || !strings.Contains(ref.msg, "unknown kind") {
			t.Errorf("refusal = %+v", ref)
		}
		if len(q.writes()) != 0 {
			t.Errorf("writes = %v", q.writes())
		}
	})

	t.Run("an upsert for a user type deleted since is a 400", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{getTypeErr: store.ErrNotFound})
		q := &govCovQuerier{}
		typed := g
		typed.SubjectType, typed.Subject = types.CapabilitySubjectUserType, "ghost"
		_, err := applyGrantChange(s, r, q, held("upsert", govCovGrantPayload(t, typed, false)))
		if ref := govCovAsRefusal(t, err); ref.reason != reasonAccessUnknownUserType || ref.status != http.StatusBadRequest {
			t.Errorf("refusal = %+v", ref)
		}
		if len(q.writes()) != 0 {
			t.Errorf("writes = %v", q.writes())
		}
	})

	t.Run("a failing upsert is a plain error", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{rowErr: func(sql string) error {
			if strings.Contains(sql, "INSERT INTO capability_grants") {
				return boom
			}
			return pgx.ErrNoRows
		}}
		_, err := applyGrantChange(s, r, q, held("upsert", govCovGrantPayload(t, g, false)))
		asError(t, err)
		if !errors.Is(err, boom) {
			t.Errorf("error = %v", err)
		}
	})
}

func TestGovCovHoldAndApplyEnforcement(t *testing.T) {
	saved := types.GovernanceChange{ID: govCovChangeID}
	body := map[string]bool{capEgressHost: true, capSecret: false}
	r := govCovHumanReq(http.MethodPut, "/x", "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)

	t.Run("a replacement is held with the map as payload and each changed kind in the diff, over the nil map the skipped read leaves", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, _ := govCovServer(t, st)
		w := httptest.NewRecorder()
		s.holdEnforcement(w, r, body)
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindEnforcement || ch.Op != "replace" || ch.TargetKey != govEnforcementKey || string(ch.Payload) != `{"egress_host":true,"secret":false}` {
			t.Errorf("stored change = %+v payload %s", ch, ch.Payload)
		}
		if ch.BaseHash != computeETag(map[string]bool(nil)) {
			t.Errorf("base hash = %s, want the hash of the nil map the skipped read left", ch.BaseHash)
		}
		if d := govCovDiff(t, ch); !reflect.DeepEqual(d.Changed, []string{capEgressHost, capSecret}) {
			t.Errorf("changed = %v", d.Changed)
		}
	})

	t.Run("an If-Match that is not the hash of the map the read left is the direct write's 412 and holds nothing", func(t *testing.T) {
		st := &govCovStore{}
		s, _ := govCovServer(t, st)
		req := r.Clone(r.Context())
		req.Header.Set("If-Match", `"not-the-current-etag"`)
		w := httptest.NewRecorder()
		s.holdEnforcement(w, req, body)
		if w.Code != http.StatusPreconditionFailed || errorReason(w) != reasonCapabilityEnforcementStale || len(st.proposed) != 0 {
			t.Errorf("= %d %q, proposals %d", w.Code, errorReason(w), len(st.proposed))
		}
	})

	t.Run("with the read skipped, an If-Match equal to the hash of the nil map the read left is accepted", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, _ := govCovServer(t, st)
		req := r.Clone(r.Context())
		req.Header.Set("If-Match", computeETag(map[string]bool(nil)))
		w := httptest.NewRecorder()
		s.holdEnforcement(w, req, body)
		govCovHoldAnswer(t, w, st)
	})

	t.Run("a failing read is a 500", func(t *testing.T) {
		st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return errors.New("pg: down") }}
		s, _ := govCovServer(t, st)
		w := httptest.NewRecorder()
		s.holdEnforcement(w, r, body)
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 {
			t.Errorf("= %d, proposals %d", w.Code, len(st.proposed))
		}
	})

	t.Run("applying: an undecodable payload is an error before any statement", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		_, err := applyEnforcementChange(s, r, q, types.GovernanceChange{ID: govCovChangeID, Payload: json.RawMessage(`[1]`)})
		if err == nil || !strings.Contains(err.Error(), "payload") || len(q.seen) != 0 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("applying: a failed lock, then a failed read, each stop the apply", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		boom := errors.New("pg: down")
		q := &govCovQuerier{execFn: govCovExecFailingOn(1, boom)}
		if _, err := applyEnforcementChange(s, r, q, types.GovernanceChange{Payload: json.RawMessage(`{}`)}); !errors.Is(err, boom) || len(q.seen) != 1 {
			t.Errorf("lock failure = %v after %v", err, q.seen)
		}
		q = &govCovQuerier{queryErr: boom}
		if _, err := applyEnforcementChange(s, r, q, types.GovernanceChange{Payload: json.RawMessage(`{}`)}); !errors.Is(err, boom) || len(q.writes()) != 0 {
			t.Errorf("read failure = %v, writes %v", err, q.writes())
		}
	})
}

func TestGovCovHoldAndApplyAvailability(t *testing.T) {
	saved := types.GovernanceChange{ID: govCovChangeID}
	r := govCovHumanReq(http.MethodPut, "/x", "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)
	boom := errors.New("pg: down")

	t.Run("a restriction is held with the bit as the only change", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, _ := govCovServer(t, st)
		w := httptest.NewRecorder()
		s.holdAvailability(w, r, capAgent, "claude-code", true)
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindAvailability || ch.Op != "set" || ch.TargetKey != capAgent+"/claude-code" ||
			string(ch.Payload) != `{"kind":"agent","value":"claude-code","restricted":true}` {
			t.Errorf("stored change = %+v payload %s", ch, ch.Payload)
		}
		if d := govCovDiff(t, ch); !reflect.DeepEqual(d.Changed, []string{"restricted"}) {
			t.Errorf("changed = %v", d.Changed)
		}
		// The dry run here is a no-op, so the state it leaves is the zero one; the hash must be that state's.
		if want := computeETag(availabilityState{}); ch.BaseHash != want {
			t.Errorf("base hash = %s, want the state the read left, %s", ch.BaseHash, want)
		}
	})

	t.Run("a failing read of the state is a 500", func(t *testing.T) {
		q := &govCovQuerier{queryErr: boom}
		st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := httptest.NewRecorder()
		s.holdAvailability(w, r, capAgent, "claude-code", true)
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 || len(q.seen) != 1 {
			t.Errorf("= %d, proposals %d, statements %v", w.Code, len(st.proposed), q.seen)
		}
	})

	t.Run("availabilityStateQ surfaces the restriction read's failure", func(t *testing.T) {
		q := &govCovQuerier{queryErr: boom}
		if _, err := availabilityStateQ(r, q, capAgent, "x"); !errors.Is(err, boom) || len(q.seen) != 1 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("applying: an undecodable payload is an error before any statement", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		_, err := applyAvailabilityChange(s, r, q, types.GovernanceChange{ID: govCovChangeID, Payload: json.RawMessage(`{"kind":"agent","extra":1}`)})
		if err == nil || !strings.Contains(err.Error(), "payload") || len(q.seen) != 0 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("applying: a failed lock and a failed state read each stop the apply", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		ch := types.GovernanceChange{Payload: json.RawMessage(`{"kind":"agent","value":"x","restricted":true}`)}
		q := &govCovQuerier{execFn: govCovExecFailingOn(1, boom)}
		if _, err := applyAvailabilityChange(s, r, q, ch); !errors.Is(err, boom) || len(q.seen) != 1 {
			t.Errorf("lock failure = %v after %v", err, q.seen)
		}
		q = &govCovQuerier{queryErr: boom}
		if _, err := applyAvailabilityChange(s, r, q, ch); !errors.Is(err, boom) || len(q.writes()) != 0 {
			t.Errorf("read failure = %v, writes %v", err, q.writes())
		}
	})
}

func TestGovCovHoldAndApplyUserType(t *testing.T) {
	saved := types.GovernanceChange{ID: govCovChangeID}
	r := govCovHumanReq(http.MethodPut, "/x", "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)
	want := types.UserType{ID: "contractor", Name: "Contractor", Description: "d", Priority: 5}

	t.Run("a priority edit is held with the new type as payload and after view", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, _ := govCovServer(t, st)
		w := httptest.NewRecorder()
		s.holdUserTypeUpdate(w, r, want)
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindUserType || ch.Op != "update" || ch.TargetKey != "contractor" ||
			string(ch.Payload) != `{"id":"contractor","name":"Contractor","description":"d","priority":5}` {
			t.Errorf("stored change = %+v payload %s", ch, ch.Payload)
		}
		d := govCovDiff(t, ch)
		if string(d.After) != `{"name":"Contractor","description":"d","priority":5}` || !strings.Contains(strings.Join(d.Changed, ","), "priority") {
			t.Errorf("diff = %s", ch.Diff)
		}
	})

	for _, c := range []struct {
		name       string
		dryRun     error
		wantCode   int
		wantReason string
	}{
		{"a missing type is the direct write's 404", store.ErrNotFound, http.StatusNotFound, reasonUserTypeNotFound},
		{"a name clash is the direct write's 409", store.ErrConflict, http.StatusConflict, reasonUserTypeConflict},
		{"a store failure is a 500", errors.New("pg: down"), http.StatusInternalServerError, reasonInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return c.dryRun }}
			s, _ := govCovServer(t, st)
			w := httptest.NewRecorder()
			s.holdUserTypeUpdate(w, r, want)
			if w.Code != c.wantCode || errorReason(w) != c.wantReason || len(st.proposed) != 0 {
				t.Errorf("= %d %q, proposals %d", w.Code, errorReason(w), len(st.proposed))
			}
		})
	}

	t.Run("the dry run reads the type before it tries the update", func(t *testing.T) {
		q := &govCovQuerier{}
		st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := httptest.NewRecorder()
		s.holdUserTypeUpdate(w, r, want)
		if w.Code != http.StatusNotFound || len(q.seen) != 1 || len(q.writes()) != 0 {
			t.Errorf("= %d after %v", w.Code, q.seen)
		}
	})

	t.Run("applying: an undecodable payload is an error before any statement", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		_, err := applyUserTypeChange(s, r, q, types.GovernanceChange{ID: govCovChangeID, Payload: json.RawMessage(`{"extra":1}`)})
		if err == nil || !strings.Contains(err.Error(), "payload") || len(q.seen) != 0 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("applying: a type deleted since the proposal is a 404", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		q := &govCovQuerier{}
		_, err := applyUserTypeChange(s, r, q, types.GovernanceChange{TargetKey: "contractor", Payload: json.RawMessage(`{"name":"x"}`)})
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusNotFound || ref.reason != reasonUserTypeNotFound || len(q.writes()) != 0 {
			t.Errorf("refusal = %+v, writes %v", ref, q.writes())
		}
	})

	t.Run("applying: a failed read is returned and nothing is written", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{})
		boom := errors.New("pg: down")
		q := &govCovQuerier{rowErr: func(string) error { return boom }}
		_, err := applyUserTypeChange(s, r, q, types.GovernanceChange{TargetKey: "contractor", Payload: json.RawMessage(`{"name":"x"}`)})
		var ref *profileWriteError
		if !errors.Is(err, boom) || errors.As(err, &ref) || len(q.writes()) != 0 {
			t.Errorf("error = %v, writes %v", err, q.writes())
		}
	})
}
