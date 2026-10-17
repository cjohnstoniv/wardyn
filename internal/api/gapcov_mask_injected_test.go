// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const gapCovSecretValue = "a-credential-value-long-enough-to-mask"

// A value the shared registry cannot commit is not on record: maskInjected says so and the value is
// not masked locally either, so a retry commits it again.
func TestGapCovMaskInjectedReportsAValueThatCannotBeCommitted(t *testing.T) {
	boom := errors.New("gapcov: commit refused")
	b := &gapCovMaskBackend{putRunErr: boom}
	reg := secretmask.NewRegistry()
	reg.SetBackend(b)
	s := &Server{cfg: Config{MaskRegistry: reg}}
	run := uuid.MustParse("00000000-0000-0000-0000-00000000f001")

	if err := s.maskInjected(run, []byte(gapCovSecretValue)); !errors.Is(err, boom) {
		t.Fatalf("maskInjected = %v, want the commit error", err)
	}
	if got := reg.Masker(run).Mask([]byte("x " + gapCovSecretValue + " y")); string(got) != "x "+gapCovSecretValue+" y" {
		t.Errorf("a value that was never committed is masked locally: %q", got)
	}
	b.putRunErr = nil
	if err := s.maskInjected(run, []byte(gapCovSecretValue)); err != nil {
		t.Fatalf("retry = %v, want the commit to succeed", err)
	}
	if got := string(reg.Masker(run).Mask([]byte("x " + gapCovSecretValue + " y"))); got == "x "+gapCovSecretValue+" y" {
		t.Error("the value is not masked after a successful commit")
	}
}

// A credential that cannot be recorded for masking is not handed to the run: the door answers 503
// mask_state_unavailable and audits the denial as the run's own.
func TestGapCovRefuseUnmaskedAnswers503AndAuditsTheRun(t *testing.T) {
	run := uuid.MustParse("00000000-0000-0000-0000-00000000f002")
	claims := &identity.Claims{RunID: run, SPIFFEID: "spiffe://wardyn.local/agent-run/" + run.String()}

	t.Run("a commit that fails", func(t *testing.T) {
		reg := secretmask.NewRegistry()
		reg.SetBackend(&gapCovMaskBackend{putRunErr: errors.New("gapcov: commit refused")})
		h := newHarness(t)
		cfg := baseTestConfig(h, nil)
		cfg.MaskRegistry = reg
		s := New(cfg)

		w := httptest.NewRecorder()
		if !s.refuseUnmasked(w, httptest.NewRequest(http.MethodPost, "/x", nil), claims, "creds.mint", []byte(gapCovSecretValue)) {
			t.Fatal("refuseUnmasked = false, want it to refuse")
		}
		if w.Code != authz.EffectUnavailable.Status() || errorReason(w) != string(authz.ReasonMaskStateUnavailable) {
			t.Fatalf("answer = %d %q, want 503 %s", w.Code, errorReason(w), authz.ReasonMaskStateUnavailable)
		}
		rows := govCovAudits(h, authz.AuditAction)
		if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].ActorType != types.ActorAgent || rows[0].Actor != claims.SPIFFEID {
			t.Fatalf("denial rows = %+v, want one denied row as the run's agent identity", rows)
		}
	})
	t.Run("a commit that succeeds", func(t *testing.T) {
		s := &Server{cfg: Config{MaskRegistry: secretmask.NewRegistry()}}
		w := httptest.NewRecorder()
		if s.refuseUnmasked(w, httptest.NewRequest(http.MethodPost, "/x", nil), claims, "creds.mint", []byte(gapCovSecretValue)) || w.Body.Len() != 0 {
			t.Fatalf("refused (%d %q) a value that was recorded", w.Code, w.Body)
		}
	})
}
