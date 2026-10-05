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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	govCovMapEng = uuid.MustParse("ffffffff-0000-0000-0000-000000000001")
	govCovMapOps = uuid.MustParse("ffffffff-0000-0000-0000-000000000002")
)

func govCovMappingRows() []types.RoleMapping {
	return []types.RoleMapping{
		{ID: govCovMapEng, Value: "eng", Role: oidc.RoleUser, UserType: types.UserTypeStandard},
		{ID: govCovMapOps, Value: "ops-group", Role: oidc.RoleAdmin},
	}
}

// govCovAccessServer is a server over st with a real authenticator, whose session cookies
// govCovAccessCookie signs.
func govCovAccessServer(t *testing.T, st store.Store, roleMap map[string]string) *Server {
	t.Helper()
	auth := newAccessAuth(t, roleMap, "", nil, nil)
	s, _ := govCovServer(t, st, func(c *Config) { c.OIDC = auth })
	return s
}

func govCovAccessCookie(t *testing.T, role string, groups ...string) *http.Cookie {
	t.Helper()
	return accessSession(t, "sub-super", "super@corp.example", role, groups)
}

func TestGovCovRoleMappingStateAndView(t *testing.T) {
	rows := govCovMappingRows()
	if got := roleMappingState(rows, "eng").(map[string]any); got["mapping"].(types.RoleMapping).ID != govCovMapEng {
		t.Errorf("state = %v", got)
	}
	if got := roleMappingState(rows, "nope").(map[string]any); got["mapping"] != nil {
		t.Errorf("state of an absent value = %v, want a nil mapping", got)
	}
	if computeETag(roleMappingState(rows, "eng")) == computeETag(roleMappingState(rows, "nope")) {
		t.Error("a mapping appearing must change the base hash")
	}
	if v := roleMappingViewOf(rows, "eng"); v == nil || v.Role != oidc.RoleUser || v.UserType != types.UserTypeStandard || v.Value != "eng" {
		t.Errorf("view = %+v", v)
	}
	if roleMappingViewOf(rows, "nope") != nil {
		t.Error("an absent value has no view")
	}
}

func TestGovCovHoldRoleMappingUpsert(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	saved := types.GovernanceChange{ID: govCovChangeID}
	super := func(t *testing.T) *http.Cookie { return govCovAccessCookie(t, oidc.RoleAdmin, "ops-group") }

	t.Run("a new value is held on its canonical form with no before view", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved, roleMappings: govCovMappingRows()}
		s := govCovAccessServer(t, st, nil)
		w := doSSO(t, s, http.MethodPost, "/api/v1/access/mappings", super(t), `{"value":"  NEW-Group ","role":"user"}`)
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindRoleMapping || ch.Op != "upsert" || ch.TargetKey != "new-group" {
			t.Errorf("stored change = %+v", ch)
		}
		var payload roleMappingWriteRequest
		if err := json.Unmarshal(ch.Payload, &payload); err != nil || payload.Value != "new-group" || payload.Role != oidc.RoleUser {
			t.Errorf("payload = %s (%v), want the canonical value replayed", ch.Payload, err)
		}
		if want := computeETag(roleMappingState(govCovMappingRows(), "new-group")); ch.BaseHash != want {
			t.Errorf("base hash = %s, want the hash of 'no such mapping'", ch.BaseHash)
		}
		d := govCovDiff(t, ch)
		if (len(d.Before) != 0 && string(d.Before) != "null") || string(d.After) != `{"value":"new-group","role":"user","user_type":"standard"}` {
			t.Errorf("diff = %s", ch.Diff)
		}
	})

	t.Run("a change to an existing value carries its before view", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved, roleMappings: govCovMappingRows()}
		s := govCovAccessServer(t, st, nil)
		w := doSSO(t, s, http.MethodPost, "/api/v1/access/mappings", super(t), `{"value":"eng","role":"security_admin"}`)
		ch := govCovHoldAnswer(t, w, st)
		d := govCovDiff(t, ch)
		if string(d.Before) != `{"value":"eng","role":"user","user_type":"standard"}` || !reflect.DeepEqual(d.Changed, []string{"role", "user_type"}) {
			t.Errorf("diff = %s", ch.Diff)
		}
		if ch.BaseHash != computeETag(roleMappingState(govCovMappingRows(), "eng")) {
			t.Errorf("base hash does not cover the row the proposal saw")
		}
	})

	t.Run("a change that would lock the proposer out is held, not refused: the guard is the approver's", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved, roleMappings: govCovMappingRows()}
		s := govCovAccessServer(t, st, nil)
		w := doSSO(t, s, http.MethodPost, "/api/v1/access/mappings", super(t), `{"value":"ops-group","role":"user"}`)
		if w.Code != http.StatusAccepted || len(st.proposed) != 1 {
			t.Errorf("= %d %s, proposals %d", w.Code, w.Body.String(), len(st.proposed))
		}
	})

	t.Run("a value the chart already sets is refused with its cause", func(t *testing.T) {
		st := &govCovStore{}
		s := govCovAccessServer(t, st, map[string]string{"eng": "admin"})
		w := doSSO(t, s, http.MethodPost, "/api/v1/access/mappings", super(t), `{"value":"eng","role":"user"}`)
		body := govCovDecode(t, w)
		if w.Code != http.StatusBadRequest || body["cause"] != "chart" || body["value"] != "eng" ||
			!strings.Contains(body["error"].(string), "already set by your chart config") || len(st.proposed) != 0 {
			t.Errorf("= %d %v, proposals %d", w.Code, body, len(st.proposed))
		}
	})

	for _, c := range []struct {
		name       string
		body       string
		roleMap    map[string]string
		st         *govCovStore
		wantCode   int
		wantReason string
	}{
		{"a role that is not one", `{"value":"x","role":"emperor"}`, nil, &govCovStore{}, 400, reasonAccessMappingTargetInvalid},
		{"an email value with email mappings off", `{"value":"a@corp.example","role":"user"}`, nil, &govCovStore{}, 400, reasonAccessEmailMappingDisabled},
		{"a mapping list that cannot be read", `{"value":"x","role":"user"}`, nil, &govCovStore{listRolesErr: errors.New("pg: down")}, 500, reasonInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := govCovAccessServer(t, c.st, c.roleMap)
			w := doSSO(t, s, http.MethodPost, "/api/v1/access/mappings", super(t), c.body)
			if w.Code != c.wantCode || errorReason(w) != c.wantReason || len(c.st.proposed) != 0 {
				t.Errorf("= %d %q (%s), proposals %d", w.Code, errorReason(w), w.Body.String(), len(c.st.proposed))
			}
		})
	}
}

func TestGovCovHoldRoleMappingDelete(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	saved := types.GovernanceChange{ID: govCovChangeID}
	super := func(t *testing.T) *http.Cookie { return govCovAccessCookie(t, oidc.RoleAdmin, "ops-group") }

	t.Run("a delete by id is held on the value it resolved to", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved, roleMappings: govCovMappingRows()}
		s := govCovAccessServer(t, st, nil)
		w := doSSO(t, s, http.MethodDelete, "/api/v1/access/mappings/"+govCovMapEng.String()+"?acknowledge_access_change=true", super(t), "")
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindRoleMapping || ch.Op != "delete" || ch.TargetKey != "eng" {
			t.Errorf("stored change = %+v", ch)
		}
		if string(ch.Payload) != `{"id":"`+govCovMapEng.String()+`","acknowledge_access_change":true}` {
			t.Errorf("payload = %s, want the id and the acknowledgement it was proposed with", ch.Payload)
		}
		d := govCovDiff(t, ch)
		if string(d.Before) != `{"value":"eng","role":"user","user_type":"standard"}` || d.After != nil {
			t.Errorf("diff = %s", ch.Diff)
		}
		if ch.BaseHash != computeETag(roleMappingState(govCovMappingRows(), "eng")) {
			t.Error("base hash does not cover the row the proposal saw")
		}
	})

	t.Run("an unknown id is a 404 and holds nothing", func(t *testing.T) {
		st := &govCovStore{roleMappings: govCovMappingRows()}
		s := govCovAccessServer(t, st, nil)
		w := doSSO(t, s, http.MethodDelete, "/api/v1/access/mappings/"+uuid.NewString(), super(t), "")
		if w.Code != http.StatusNotFound || errorReason(w) != reasonRoleMappingNotFound || len(st.proposed) != 0 {
			t.Errorf("= %d %q, proposals %d", w.Code, errorReason(w), len(st.proposed))
		}
	})
}

func TestGovCovRefusalOfIgnoresTheRequest(t *testing.T) {
	ref := refusalOf("why", func(w http.ResponseWriter) { writeErrorReason(w, http.StatusTeapot, "r", "m") })
	if ref.Error() != "governance change refused: why" {
		t.Errorf("Error() = %q", ref.Error())
	}
	w := httptest.NewRecorder()
	ref.write(w, nil)
	if w.Code != http.StatusTeapot || errorReason(w) != "r" {
		t.Errorf("write = %d %q", w.Code, errorReason(w))
	}
}

func TestGovCovApplyRoleMappingChangeStopsEarly(t *testing.T) {
	boom := errors.New("pg: down")
	ch := types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindRoleMapping, TargetKey: "eng", Op: "upsert"}
	r := govCovHumanReq(http.MethodPost, "/x", "sub-super", "super@corp.example", oidc.RoleAdmin)

	t.Run("without SSO the approval is refused", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{}, func(c *Config) { c.OIDC = nil })
		q := &govCovQuerier{}
		_, err := applyRoleMappingChange(s, r, q, ch)
		var ref *govRefusal
		if !errors.As(err, &ref) {
			t.Fatalf("error = %v, want a refusal", err)
		}
		w := httptest.NewRecorder()
		ref.write(w, r)
		if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonSSONotConfigured || len(q.seen) != 0 {
			t.Errorf("= %d %q after %v", w.Code, errorReason(w), q.seen)
		}
	})

	t.Run("a failed target lock stops the apply", func(t *testing.T) {
		s := govCovAccessServer(t, &govCovStore{}, nil)
		q := &govCovQuerier{execFn: govCovExecFailingOn(1, boom)}
		if _, err := applyRoleMappingChange(s, r, q, ch); !errors.Is(err, boom) || len(q.seen) != 1 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})

	t.Run("a failed read of the mappings stops the apply before any write", func(t *testing.T) {
		s := govCovAccessServer(t, &govCovStore{}, nil)
		q := &govCovQuerier{queryErr: boom}
		if _, err := applyRoleMappingChange(s, r, q, ch); !errors.Is(err, boom) || len(q.writes()) != 0 || len(q.seen) != 2 {
			t.Errorf("error = %v after %v", err, q.seen)
		}
	})
}

func govCovDeleteChange(acknowledge bool) types.GovernanceChange {
	payload, _ := json.Marshal(roleMappingDeletePayload{ID: govCovMapEng, AcknowledgeAccessChange: acknowledge})
	return types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindRoleMapping, TargetKey: "eng", Op: "delete", Payload: payload}
}

func TestGovCovApplyRoleMappingDelete(t *testing.T) {
	userTypes := []types.UserType{{ID: types.UserTypeStandard, Name: "Standard user", BuiltIn: true}}
	rows := govCovMappingRows()
	approver := func(groups ...string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		return r.WithContext(withHumanIdentity(r.Context(), "sub-super", "super@corp.example", oidc.RoleAdmin, types.UserTypeStandard, groups, false))
	}

	t.Run("a payload that does not decode is a 400", func(t *testing.T) {
		s := govCovAccessServer(t, &govCovStore{}, nil)
		ch := govCovDeleteChange(false)
		ch.Payload = json.RawMessage(`{"id":1`)
		_, err := s.applyRoleMappingDelete(approver("ops-group"), &govCovQuerier{}, ch, rows, userTypes)
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusBadRequest || ref.reason != reasonInvalidRequestBody {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("the guard is the approver's: a delete that strips their own admin is refused and nothing is written", func(t *testing.T) {
		s := govCovAccessServer(t, &govCovStore{}, nil)
		q := &govCovQuerier{}
		ch := govCovDeleteChange(true)
		payload, _ := json.Marshal(roleMappingDeletePayload{ID: govCovMapOps, AcknowledgeAccessChange: true})
		ch.Payload = payload
		_, err := s.applyRoleMappingDelete(approver("ops-group"), q, ch, rows, userTypes)
		var ref *govRefusal
		if !errors.As(err, &ref) {
			t.Fatalf("error = %v, want a refusal", err)
		}
		w := httptest.NewRecorder()
		ref.write(w, nil)
		if w.Code != http.StatusBadRequest || errorReason(w) != reasonAccessLockout || !strings.Contains(w.Body.String(), "remove your own admin access") {
			t.Errorf("= %d %q %s", w.Code, errorReason(w), w.Body.String())
		}
		if len(q.seen) != 0 {
			t.Errorf("statements after a lockout refusal: %v", q.seen)
		}
	})

	t.Run("a row that is already gone is the direct delete's 404", func(t *testing.T) {
		s := govCovAccessServer(t, &govCovStore{}, nil)
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 0")}
		_, err := s.applyRoleMappingDelete(approver("ops-group"), q, govCovDeleteChange(false), rows, userTypes)
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusNotFound || ref.reason != reasonRoleMappingNotFound {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("a failing delete statement is a plain error", func(t *testing.T) {
		s := govCovAccessServer(t, &govCovStore{}, nil)
		boom := errors.New("pg: down")
		q := &govCovQuerier{execFn: func(string) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, boom }}
		_, err := s.applyRoleMappingDelete(approver("ops-group"), q, govCovDeleteChange(false), rows, userTypes)
		var ref *profileWriteError
		if !errors.Is(err, boom) || errors.As(err, &ref) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a delete applies, audits the row it removed and reports what it did to tokens after commit", func(t *testing.T) {
		stale := types.APIToken{ID: uuid.New(), Principal: "dev", Groups: []string{"eng"}, UserType: "other"}
		st := &govCovStore{apiTokens: []types.APIToken{stale}}
		s := govCovAccessServer(t, st, nil)
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 1")}
		got, err := s.applyRoleMappingDelete(approver("ops-group"), q, govCovDeleteChange(false), rows, userTypes)
		if err != nil {
			t.Fatal(err)
		}
		if got.action != "access.role_mapping.delete" || got.target != govCovMapEng.String() ||
			!reflect.DeepEqual(got.data, map[string]any{"value": "eng", "role": oidc.RoleUser, "user_type": types.UserTypeStandard}) {
			t.Errorf("applied = %+v", got)
		}
		if w := q.writes(); len(w) != 1 || !strings.Contains(w[0], "DELETE FROM role_mappings") {
			t.Errorf("writes = %v", w)
		}
		after := got.afterCommit()
		if after["stale_token_snapshots"] != 1 || after["tokens_revoked"] != 0 || after["tokens_revocation_failed"] != nil {
			t.Errorf("after-commit data = %v, want one stale snapshot counted and nothing revoked", after)
		}
	})

	t.Run("a token list that cannot be read is reported as a failed revocation", func(t *testing.T) {
		st := &govCovStore{apiTokensErr: errors.New("pg: down")}
		s := govCovAccessServer(t, st, nil)
		q := &govCovQuerier{execFn: govCovExecTag("DELETE 1")}
		got, err := s.applyRoleMappingDelete(approver("ops-group"), q, govCovDeleteChange(false), rows, userTypes)
		if err != nil {
			t.Fatal(err)
		}
		after := got.afterCommit()
		if after["tokens_revocation_failed"] != true || after["tokens_revoked"] != 0 || after["stale_token_snapshots"] != 0 {
			t.Errorf("after-commit data = %v, want a failed revocation and no counts", after)
		}
	})
}
