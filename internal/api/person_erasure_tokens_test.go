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
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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
	mayWork := func(t *testing.T, e erased) {
		t.Helper()
		if e.code != http.StatusOK || e.outcome != "success" {
			t.Fatalf("erase = %d, row %s; want 200 and success", e.code, e.outcome)
		}
		for what, got := range map[string]map[string]any{"answer": e.answer, "audit row": e.audited} {
			if got["tokens_revocation_failed"] != true || got["detail"] != erasureTokensNotRevoked {
				t.Errorf("%s = %v, want tokens_revocation_failed and the remedy", what, got)
			}
		}
	}
	for route, erase := range routes {
		// A refused token's row is closed, so the retry of an erase that did not finish no longer revokes
		// it, but the token may still work: the retry says so too, after a later failure in the same erase
		// and beside a revoke that did not finish.
		t.Run(route+", refused, then a later failure", func(t *testing.T) {
			fx := newADOConsoleLane(t, true)
			fail := errors.New("login groups unreachable")
			fx.srv.cfg.Store = loginGroupsFailing{adoConsoleStore{fx.st}, &fail}
			fx.pats.revokeErr = &adoPATError{Status: http.StatusBadRequest}
			if e := erase(t, fx); e.code == http.StatusOK || e.outcome != "failure" {
				t.Fatalf("erase = %d, row %s; want a failure", e.code, e.outcome)
			}
			fail, fx.pats.revokeErr = nil, nil
			mayWork(t, erase(t, fx))
		})
		t.Run(route+", refused beside unfinished", func(t *testing.T) {
			fx := newADOConsoleLane(t, true)
			// A second run's token, in an organisation the fake refuses (400), beside the run's own (503).
			p := fx.st.unrevoked(t)[0]
			p.RunID, p.AuthorizationID, p.Org = uuid.New(), uuid.New(), "fabrikam"
			if err := fx.st.InsertRunPAT(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			fx.pats.revokeErr = &adoPATError{Status: http.StatusServiceUnavailable}
			if e := erase(t, fx); e.code != http.StatusServiceUnavailable || e.reason != reasonCredentialEraseRevokeUnfinished {
				t.Fatalf("erase = %d %q; want 503 %s", e.code, e.reason, reasonCredentialEraseRevokeUnfinished)
			}
			fx.pats.revokeErr = nil
			mayWork(t, erase(t, fx))
			if open := fx.st.unrevoked(t); len(open) != 0 {
				t.Errorf("open tokens after the retry = %+v, want none", open)
			}
		})
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
					if kept {
						t.Fatal("the stored sign-in survived the erasure")
					}
					mayWork(t, e)
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

// loginGroupsFailing fails the credentials step after its revoke, at the login groups it erases next,
// while *err is set.
type loginGroupsFailing struct {
	adoConsoleStore
	err *error
}

func (s loginGroupsFailing) DeleteLoginGroups(context.Context, []string) (int64, error) {
	return 0, *s.err
}

// A purge retries an erase step that failed after a refused revoke; the retry no longer revokes the
// token, whose row is closed, but its detail still carries the flag, so the person.deprovision row says
// the token may work.
func TestSCIMPurgeEraseRetrySaysARefusedTokenMayStillWork(t *testing.T) {
	fx := newADOConsoleLane(t, true)
	fail := errors.New("login groups unreachable")
	fx.srv.cfg.Store = loginGroupsFailing{adoConsoleStore{fx.st}, &fail}
	fx.pats.revokeErr = &adoPATError{Status: http.StatusBadRequest}
	if _, err := fx.srv.eraseForPurge(context.Background(), fx.subject); err == nil {
		t.Fatal("eraseForPurge = nil, want the login groups failure")
	}
	fail, fx.pats.revokeErr = nil, nil
	if detail, err := fx.srv.eraseForPurge(context.Background(), fx.subject); err != nil || detail["tokens_revocation_failed"] != 1 {
		t.Errorf("the retried eraseForPurge = %v, %v; want the flag: the refused token may still work", detail, err)
	}
}

// tokenHistoryLeft keeps token history as store.PG does: each erase leaves left of the person's token
// rows, and the sweep's deletes of what an erase kept are recorded in swept.
type tokenHistoryLeft struct {
	adoConsoleStore
	left  int64
	swept *[]string
}

func (s tokenHistoryLeft) DeleteSettledRunPATsOf(context.Context, []string) (int64, int64, error) {
	return 0, s.left, nil
}

func (tokenHistoryLeft) DeleteDelegatedTokensOf(context.Context, []string) (int64, error) {
	return 0, nil
}

func (s tokenHistoryLeft) DeleteErasedRefusedRunPATs(_ context.Context, eraseReason string) (int64, error) {
	if s.swept != nil {
		*s.swept = append(*s.swept, eraseReason)
	}
	return 0, nil
}

// A purge's erase step counts the token rows its erase left, as the erase routes do, so the purge's
// person.deprovision row says how many of the person's tokens may still work.
func TestSCIMPurgeEraseCountsTheTokenRowsLeft(t *testing.T) {
	fx := newADOConsoleLane(t, true)
	fx.srv.cfg.Store = tokenHistoryLeft{adoConsoleStore: adoConsoleStore{fx.st}, left: 2}
	if detail, err := fx.srv.eraseForPurge(context.Background(), fx.subject); err != nil || detail["live_left"] != 2 {
		t.Errorf("eraseForPurge = %v, %v; want live_left 2, the token rows the erase left", detail, err)
	}
}

// The purge's person.deprovision row carries the flag and the remedy when an erase step's ledger detail
// has it, and neither otherwise, and live_left as the erase steps counted it.
func TestSCIMPurgeRowSaysWhenATokenWasNotRevoked(t *testing.T) {
	for _, refused := range []bool{true, false} {
		cov := newSCIMCovStore()
		l := scimCovSeedPurgee(cov)
		if refused {
			cov.jobs = append(cov.jobs, &store.DeprovisionJob{IdentityID: l.id, Kind: store.JobKindPurge, Step: jobStepErase,
				Target: "sub-pat", Done: true, Detail: map[string]int{"tokens_revocation_failed": 1, "live_left": 1}})
		}
		e := newSCIMCovEnv(t, cov)
		if w := e.scim(http.MethodDelete, "/scim/v2/Users/"+l.id.String(), ""); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
		}
		got := e.auditData(e.purgeRow())
		if said := got["tokens_revocation_failed"] == true && got["detail"] == erasureTokensNotRevoked; said != refused {
			t.Errorf("refused=%v: person.deprovision = %v, want the flag and the remedy only after a refused revoke", refused, got)
		}
		if left := got["live_left"] == float64(1); left != refused {
			t.Errorf("refused=%v: person.deprovision live_left = %v, want the erase steps' count (1, or 0 with none)", refused, got["live_left"])
		}
	}
}

// Retiring a service identity has no refused revoke to report: no run token is created in its name.
func TestServiceIdentityOwnsNoADORunToken(t *testing.T) {
	if got := adoLaneOwner("service:ci-nightly", false); got != "" {
		t.Errorf("adoLaneOwner(service:ci-nightly) = %q, want \"\": the lane declines a service identity", got)
	}
}

// entraFormStore proves one entra:<tenant>:<object> form the person's own, beside their principal: a
// name an install upgraded from 0.8.9 can still hold credentials, and so run tokens, under.
type entraFormStore struct {
	adoConsoleStore
	person, form string
}

func (entraFormStore) RunIDsCreatedBy(context.Context, string) ([]uuid.UUID, error) { return nil, nil }
func (entraFormStore) BlankRunTasks(context.Context, string) (int, error)           { return 0, nil }
func (entraFormStore) PrincipalAliases(context.Context, string) ([]string, error)   { return nil, nil }
func (entraFormStore) PrincipalsSharingObject(context.Context, string) ([]string, error) {
	return nil, nil
}

func (entraFormStore) EraseGovernanceChangePersonalFields(context.Context, []string, []string) (int, error) {
	return 0, nil
}

func (entraFormStore) SubjectKeyDestroyedSince(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func (s entraFormStore) PrincipalEntraNames(_ context.Context, principal string) ([]string, error) {
	if principal == s.person {
		return []string{s.form}, nil
	}
	return nil, nil
}

func (s entraFormStore) PrincipalsNamed(_ context.Context, name string) ([]string, error) {
	if name == s.person || name == s.form {
		return []string{s.person}, nil
	}
	return nil, nil
}

// A run token recorded under an entra:<tenant>:<object> form the person holds credentials under is
// theirs too: the erase tries to revoke it with the rest, and reads one a revoke refused earlier.
// No sign-in is stored under the form here, so its revoke is refused for good.
func TestPersonErasure_ReachesTokensUnderAnEntraForm(t *testing.T) {
	const form = "entra:11111111-1111-4111-8111-111111111111:22222222-2222-4222-8222-222222222222"
	underForm := func(t *testing.T, fx *adoPATFixture) store.RunPAT {
		t.Helper()
		fx.srv.cfg.Store = entraFormStore{adoConsoleStore{fx.st}, fx.subject, form}
		p := fx.st.unrevoked(t)[0]
		p.RunID, p.AuthorizationID, p.Owner = uuid.New(), uuid.New(), form
		if err := fx.st.InsertRunPAT(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	erase := func(t *testing.T, fx *adoPATFixture) {
		t.Helper()
		var rep secretstore.EraseReport
		if _, failed, err := fx.srv.eraseCredentialsScope(context.Background(), fx.subject, nil, &rep); err != nil || !failed {
			t.Fatalf("eraseCredentialsScope = flag %v, %v; want the flag and no error: a token under the entra form may still work", failed, err)
		}
	}
	t.Run("still open", func(t *testing.T) {
		fx := newADOConsoleLane(t, true)
		underForm(t, fx)
		erase(t, fx)
		if open := fx.st.unrevoked(t); len(open) != 0 {
			t.Errorf("open tokens after the erase = %+v, want none: the one under the entra form was not tried", open)
		}
	})
	t.Run("refused earlier", func(t *testing.T) {
		fx := newADOConsoleLane(t, true)
		p := underForm(t, fx)
		if _, err := fx.st.MarkRunPATRevoked(context.Background(), p.RunID, p.AuthorizationID, adoPATRevokeRunEnd, "HTTP 403"); err != nil {
			t.Fatal(err)
		}
		erase(t, fx)
	})
}
