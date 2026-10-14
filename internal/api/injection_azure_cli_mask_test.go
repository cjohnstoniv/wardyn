// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// The Azure CLI resolve door hands out its bearer token only once the run's masking has it on record: a
// backend that cannot commit the value gets a 503 mask_state_unavailable, no token and no success row.
func TestAzureCLIResolve_RefusesATokenMaskingCouldNotRecord(t *testing.T) {
	resolve := func(t *testing.T, backend *gapCovMaskBackend) (*azCLIFixture, string, int, string, string) {
		t.Helper()
		f := newAzCLIFixture(t, types.ComponentAccessRead, azCLISubA)
		var token string
		f.fake.OnIssue(func(it entrafake.IssuedToken) { token = it.AccessToken })
		f.signIn(t)
		f.srv.cfg.MaskRegistry.SetBackend(backend)
		inj, _, _, _, _ := f.dispatch()
		w := f.resolveRule(inj[0].Rule, inj[0].GrantID, f.owner, "?phase=boot")
		return f, token, w.Code, w.Body.String(), reasonOf(t, w)
	}

	t.Run("a commit that fails", func(t *testing.T) {
		f, token, code, body, reason := resolve(t, &gapCovMaskBackend{putRunErr: errors.New("gapcov: commit refused")})
		if code != http.StatusServiceUnavailable || reason != string(authz.ReasonMaskStateUnavailable) {
			t.Fatalf("answer = %d %q, want 503 %s", code, reason, authz.ReasonMaskStateUnavailable)
		}
		if token == "" || strings.Contains(body, token) || strings.Contains(body, "Bearer") {
			t.Errorf("the refusal carries the token (%q): %s", token, body)
		}
		if rows := f.audit.find("secret.read"); len(rows) != 0 {
			t.Errorf("secret.read rows = %+v, want none for a credential never handed out", rows)
		}
	})
	t.Run("a working backend", func(t *testing.T) {
		f, token, code, body, _ := resolve(t, &gapCovMaskBackend{})
		if code != http.StatusOK || token == "" || !strings.Contains(body, "Bearer "+token) {
			t.Fatalf("answer = %d %s, want 200 carrying the token", code, body)
		}
		if rows := f.audit.find("secret.read"); len(rows) != 1 || rows[0].Outcome != "success" {
			t.Errorf("secret.read rows = %+v, want one success row", rows)
		}
	})
}
