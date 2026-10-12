// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sponsorSessionUnreadable answers the session check of one principal with an error, and every other
// principal's as the wrapped revocations do.
type sponsorSessionUnreadable struct {
	*pgTestRevocations
	sub string
}

func (r sponsorSessionUnreadable) SessionStatus(ctx context.Context, sub, email string, issue oidc.Issue, epoch int64) (oidc.SessionStatus, error) {
	if sub == r.sub {
		return 0, errors.New("session store unavailable")
	}
	return r.pgTestRevocations.SessionStatus(ctx, sub, email, issue, epoch)
}

func (r sponsorSessionUnreadable) IsSessionRevoked(ctx context.Context, sub, email string, issuedAt time.Time) (bool, error) {
	if sub == r.sub {
		return false, errors.New("session store unavailable")
	}
	return r.pgTestRevocations.IsSessionRevoked(ctx, sub, email, issuedAt)
}

// TestServiceIdentity_AuditedWhenTheStateReadFails: a create or an update that committed is audited with the
// change it made even when reading the sponsor's session afterwards fails and the request answers 500; with a
// readable session the same rows are written beside the 2xx.
func TestServiceIdentity_AuditedWhenTheStateReadFails(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := map[bool]string{false: "session_readable", true: "session_unreadable"}[failing]
		t.Run(name, func(t *testing.T) {
			e := newSvcEnv(t)
			wantCode := func(ok int) int {
				if failing {
					return http.StatusInternalServerError
				}
				return ok
			}
			auditData := func(t *testing.T, action string) map[string]any {
				t.Helper()
				var found []types.AuditEvent
				for _, ev := range e.h.audit.snapshot() {
					if ev.Action == action && ev.Target == svcPrin {
						found = append(found, ev)
					}
				}
				if len(found) != 1 {
					t.Fatalf("%s rows for %s: %d, want 1", action, svcPrin, len(found))
				}
				if found[0].Outcome != "success" {
					t.Errorf("%s outcome %q, want success", action, found[0].Outcome)
				}
				return dataOf(t, found[0])
			}

			if failing {
				e.h.srv.cfg.SessionRevocations = sponsorSessionUnreadable{pgTestRevocations: e.rev, sub: svcSponsor}
			}
			if w := e.create(t, e.sec, svcName, svcSponsor); w.Code != wantCode(http.StatusCreated) {
				t.Fatalf("create: %d %s, want %d", w.Code, w.Body.String(), wantCode(http.StatusCreated))
			}
			if _, err := e.st.GetServiceIdentity(context.Background(), svcName); err != nil {
				t.Fatalf("the created identity is not stored: %v", err)
			}
			if d := auditData(t, "service_identity.create"); d["sponsor"] != svcSponsor || d["user_type"] != types.UserTypeStandard {
				t.Errorf("service_identity.create data %v, want sponsor %q and user_type %q", d, svcSponsor, types.UserTypeStandard)
			}

			e.h.srv.cfg.SessionRevocations = e.rev
			e.mustMint(t, e.sponsor, svcName)
			if failing {
				e.h.srv.cfg.SessionRevocations = sponsorSessionUnreadable{pgTestRevocations: e.rev, sub: svcSponsor2}
			}
			w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+svcName, e.sec,
				`{"sponsor":"`+svcSponsor2+`","description":"moved"}`)
			if w.Code != wantCode(http.StatusOK) {
				t.Fatalf("sponsor change: %d %s, want %d", w.Code, w.Body.String(), wantCode(http.StatusOK))
			}
			si, err := e.st.GetServiceIdentity(context.Background(), svcName)
			if err != nil || si.Sponsor != svcSponsor2 {
				t.Fatalf("stored sponsor after the change: %q (%v), want %q", si.Sponsor, err, svcSponsor2)
			}
			d := auditData(t, "service_identity.update")
			if d["sponsor_from"] != svcSponsor || d["sponsor_to"] != svcSponsor2 || d["tokens_revoked"] != float64(1) ||
				d["description_changed"] != true || d["state"] != string(types.ServiceIdentityActive) {
				t.Errorf("service_identity.update data %v, want sponsor_from %q, sponsor_to %q, tokens_revoked 1, description_changed, state active",
					d, svcSponsor, svcSponsor2)
			}
		})
	}
}
