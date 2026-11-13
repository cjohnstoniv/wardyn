// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// How the revoke of the person's Azure DevOps tokens ended decides the erase, on both erase routes. Refused
// for good (400, 403): the erase goes on, deleting the sign-in a later revoke would need, and the answer and
// the audit row say a token may still work. Not finished (503, no answer): nothing is erased and the answer is
// 503, so the retry, with the sign-in kept, still revokes it. Unknown to Azure DevOps (404), or revoked: an
// erase that says nothing of it.
func TestPersonErasure_SaysWhenATokenWasNotRevoked(t *testing.T) {
	call := func(t *testing.T, fx *adoPATFixture, method, path, body string, h http.HandlerFunc) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("principal", fx.subject)
		w := httptest.NewRecorder()
		h(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
		var m map[string]any
		if json.Unmarshal(w.Body.Bytes(), &m) != nil {
			t.Fatalf("%s %s = %d %s, want a JSON body", method, path, w.Code, w.Body.String())
		}
		return w.Code, m
	}
	last := func(t *testing.T, fx *adoPATFixture, action string) (string, map[string]any) {
		t.Helper()
		rows := fx.audit.find(action)
		var m map[string]any
		if len(rows) == 0 || json.Unmarshal(rows[len(rows)-1].Data, &m) != nil {
			t.Fatalf("%s rows = %+v, want one", action, rows)
		}
		return rows[len(rows)-1].Outcome, m
	}
	scope := func(m map[string]any) map[string]any {
		d, _ := m["detail"].(map[string]any)
		c, _ := d["credentials"].(map[string]any)
		return c
	}
	// erased is one erase: the credentials data of the answer and of its audit row (the route's own, or
	// POST's detail.credentials), the answer's reason and the row's outcome and reason.
	type erased struct {
		code            int
		answer, audited map[string]any
		reason, outcome string
		auditedReason   any
	}
	routes := map[string]func(t *testing.T, fx *adoPATFixture) erased{
		"DELETE credentials": func(t *testing.T, fx *adoPATFixture) erased {
			code, got := call(t, fx, http.MethodDelete, "/api/v1/people/"+fx.subject+"/credentials", "", fx.srv.handleErasePersonCredentials)
			outcome, row := last(t, fx, "credential.erase")
			reason, _ := got["reason"].(string)
			return erased{code, got, row, reason, outcome, row["reason"]}
		},
		"POST erasure": func(t *testing.T, fx *adoPATFixture) erased {
			code, got := call(t, fx, http.MethodPost, "/api/v1/people/"+fx.subject+"/erasure", `{"scopes":["credentials"]}`, fx.srv.handleErasePerson)
			outcome, row := last(t, fx, "person.erasure")
			reason, _ := got["reason"].(string)
			return erased{code, scope(got), scope(row), reason, outcome, row["reason"]}
		},
	}
	clean := func(t *testing.T, e erased) {
		t.Helper()
		if e.code != http.StatusOK || e.outcome != "success" {
			t.Fatalf("erase = %d, row %s; want 200 and success", e.code, e.outcome)
		}
		for what, got := range map[string]map[string]any{"answer": e.answer, "audit row": e.audited} {
			if _, said := got["tokens_revocation_failed"]; said || got["detail"] != nil {
				t.Errorf("%s = %v, want no tokens_revocation_failed and no detail", what, got)
			}
		}
	}
	for route, erase := range routes {
		for name, revokeErr := range map[string]error{
			"refused":       &adoPATError{Status: http.StatusBadRequest},
			"no permission": &adoPATError{Status: http.StatusForbidden},
			"unfinished":    &adoPATError{Status: http.StatusServiceUnavailable},
			"no answer":     context.DeadlineExceeded,
			"gone":          &adoPATError{Status: http.StatusNotFound},
			"revoked":       nil,
		} {
			t.Run(route+", "+name, func(t *testing.T) {
				fx := newADOConsoleLane(t, true)
				fx.pats.revokeErr = revokeErr
				e := erase(t, fx)
				_, kept := fx.stored(t, fx.subject)
				switch name {
				case "refused", "no permission":
					if e.code != http.StatusOK || kept {
						t.Fatalf("erase = %d, sign-in kept %v; want 200 and the sign-in gone", e.code, kept)
					}
					for what, got := range map[string]map[string]any{"answer": e.answer, "audit row": e.audited} {
						if got["tokens_revocation_failed"] != true || got["detail"] != erasureTokensNotRevoked {
							t.Errorf("%s = %v, want tokens_revocation_failed and the remedy", what, got)
						}
					}
				case "unfinished", "no answer":
					if e.code != http.StatusServiceUnavailable || e.reason != reasonCredentialEraseRevokeUnfinished ||
						e.outcome != "failure" || e.auditedReason != reasonCredentialEraseRevokeUnfinished {
						t.Fatalf("erase = %d %q, row %s %v; want 503 and a failure row, both %s", e.code, e.reason,
							e.outcome, e.auditedReason, reasonCredentialEraseRevokeUnfinished)
					}
					if open := fx.st.unrevoked(t); !kept || len(open) == 0 {
						t.Fatalf("sign-in kept %v, open tokens %d; want both, so the retry can revoke", kept, len(open))
					}
					fx.pats.revokeErr = nil
					clean(t, erase(t, fx))
					if _, kept := fx.stored(t, fx.subject); kept || len(fx.st.unrevoked(t)) != 0 {
						t.Fatalf("after the retry: sign-in kept %v, open tokens %d; want neither", kept, len(fx.st.unrevoked(t)))
					}
				default:
					clean(t, e)
					if kept {
						t.Error("the stored sign-in survived the erasure")
					}
				}
			})
		}
	}
}

// A SCIM purge erases through the same credentials step: a refused revoke is in the erase step's ledger
// detail, and an unfinished one fails the step, keeping the sign-in for the purge's retry.
func TestSCIMPurgeEraseSaysWhenATokenWasNotRevoked(t *testing.T) {
	for name, revokeErr := range map[string]error{
		"refused":    &adoPATError{Status: http.StatusBadRequest},
		"unfinished": &adoPATError{Status: http.StatusServiceUnavailable},
		"revoked":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			fx := newADOConsoleLane(t, true)
			fx.pats.revokeErr = revokeErr
			detail, err := fx.srv.eraseForPurge(context.Background(), fx.subject)
			_, kept := fx.stored(t, fx.subject)
			switch name {
			case "refused":
				if err != nil || detail["tokens_revocation_failed"] != 1 || kept {
					t.Errorf("eraseForPurge = %v, %v, sign-in kept %v; want the flag and the sign-in gone", detail, err, kept)
				}
			case "unfinished":
				if !errors.Is(err, errADORevokeUnfinished) || !kept {
					t.Errorf("eraseForPurge = %v, sign-in kept %v; want the unfinished revoke and the sign-in kept", err, kept)
				}
			default:
				if _, said := detail["tokens_revocation_failed"]; err != nil || said || kept {
					t.Errorf("eraseForPurge = %v, %v, sign-in kept %v; want no flag and the sign-in gone", detail, err, kept)
				}
			}
		})
	}
}

// The purge's person.deprovision row carries the flag and the remedy when an erase step's ledger detail
// has it, and neither otherwise.
func TestSCIMPurgeRowSaysWhenATokenWasNotRevoked(t *testing.T) {
	for _, refused := range []bool{true, false} {
		cov := newSCIMCovStore()
		l := scimCovSeedPurgee(cov)
		if refused {
			cov.jobs = append(cov.jobs, &store.DeprovisionJob{IdentityID: l.id, Kind: store.JobKindPurge, Step: jobStepErase,
				Target: "sub-pat", Done: true, Detail: map[string]int{"tokens_revocation_failed": 1}})
		}
		e := newSCIMCovEnv(t, cov)
		if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
		}
		got := e.auditData(e.purgeRow())
		if said := got["tokens_revocation_failed"] == true && got["detail"] == erasureTokensNotRevoked; said != refused {
			t.Errorf("refused=%v: person.deprovision = %v, want the flag and the remedy only after a refused revoke", refused, got)
		}
	}
}

// Retiring a service identity has no refused revoke to report: no run token is created in its name.
func TestServiceIdentityOwnsNoADORunToken(t *testing.T) {
	if got := adoLaneOwner("service:ci-nightly", false); got != "" {
		t.Errorf("adoLaneOwner(service:ci-nightly) = %q, want \"\": the lane declines a service identity", got)
	}
}
