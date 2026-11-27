// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// failingRevocations is a session-revocation store whose sequence read (seqErr) or revocation read (readErr)
// fails.
type failingRevocations struct{ seqErr, readErr error }

func (f failingRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, f.readErr
}
func (f failingRevocations) RevokeSub(context.Context, string) error { return nil }
func (f failingRevocations) RevokeAll(context.Context) error         { return nil }
func (f failingRevocations) RevocationSeq(context.Context) (int64, error) {
	return 1, f.seqErr
}
func (f failingRevocations) IsSessionRevokedSeq(context.Context, string, string, int64) (bool, error) {
	return false, f.readErr
}

// TestPortalExchange_OperationalFailuresAudited: each read or write of this server that fails after the
// portal authenticated answers as it always did and records one delegation.exchange row, outcome failure,
// whose reason is the reply's code and whose cause names the step, with nothing of the error.
func TestPortalExchange_OperationalFailuresAudited(t *testing.T) {
	const detail = "dial tcp 10.0.0.9:5432: connection refused"
	fail := errors.New(detail)
	cases := []struct {
		name   string
		shape  func(*Config)
		set    func(*exchHarness)
		portal func(*types.Delegate)
		status int
		code   string
		desc   string
		cause  string
	}{
		{name: "revocation order", shape: func(c *Config) { c.SessionRevocations = failingRevocations{seqErr: fail} },
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", desc: "session revocation lookup failed", cause: "revocation_order_lookup"},
		{name: "session revocation", shape: func(c *Config) { c.SessionRevocations = failingRevocations{readErr: fail} },
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", desc: "session revocation lookup failed", cause: "session_revocation_lookup"},
		{name: "user type", set: func(h *exchHarness) { h.ps.userTypeErr = fail }, portal: func(d *types.Delegate) { d.UserType = "contractor" },
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", desc: "user type lookup failed", cause: "user_type_lookup"},
		{name: "sign-in record", set: func(h *exchHarness) { h.ps.groupsErr = fail },
			status: http.StatusServiceUnavailable, code: "temporarily_unavailable", desc: "sign-in record write failed", cause: "sign_in_record_write"},
		{name: "identity write", set: func(h *exchHarness) { h.ps.issueErr = fail },
			status: http.StatusInternalServerError, code: "server_error", desc: "issue portal identity failed", cause: "issue_portal_identity"},
		{name: "grant write", set: func(h *exchHarness) { h.ps.mintErr = fail },
			status: http.StatusInternalServerError, code: "server_error", desc: "mint portal grant failed", cause: "mint_portal_grant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var shapes []func(*Config)
			if tc.shape != nil {
				shapes = append(shapes, tc.shape)
			}
			h := newExchHarness(t, shapes...)
			if tc.set != nil {
				tc.set(h)
			}
			d, cred := h.seedPortal(tc.portal)
			w := h.exchange(t, d.ID, cred, "subject-token", nil)
			if w.Code != tc.status || oauthCode(w) != tc.code || !strings.Contains(w.Body.String(), tc.desc) {
				t.Fatalf("exchange = %d %s, want %d %s %q", w.Code, w.Body.String(), tc.status, tc.code, tc.desc)
			}
			rows := h.exchangeRows()
			if len(rows) != 1 {
				t.Fatalf("delegation.exchange rows = %+v, want one", rows)
			}
			ev := rows[0]
			data := portalRowData(t, ev)
			if ev.Outcome != "failure" || ev.Actor != delegateActor(d.ID) || ev.Target != h.ver.subj.Principal ||
				data["reason"] != tc.code || data["cause"] != tc.cause || data["issuer"] != d.Issuer {
				t.Fatalf("failure row = %+v %v, want outcome failure, reason %s, cause %s, the portal, the person and the issuer", ev, data, tc.code, tc.cause)
			}
			if strings.Contains(string(ev.Data), "10.0.0.9") || strings.Contains(string(ev.Data), "refused") {
				t.Fatalf("failure row %s carries the error", ev.Data)
			}
		})
	}
}
