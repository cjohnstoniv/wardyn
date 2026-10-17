// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Azure DevOps run token's renderings on the run masking manifest, against
// a real Postgres (see mask_manifest_pg_test.go).

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maskedADOLane is the Azure DevOps minted_pat fixture with its run persisted in
// Postgres (the manifest's foreign key) and a registry plus manifests wired.
type maskedADOLane struct {
	*adoPATFixture
	l   *maskLab
	reg *secretmask.Registry
	m   *maskmanifest.Manifests
}

func newMaskedADOLane(t *testing.T) *maskedADOLane {
	t.Helper()
	fx := newADOPATLane(t)
	l := newMaskLab(t)
	now := time.Now().UTC()
	r := fx.run
	r.CreatedAt, r.UpdatedAt, r.Agent, r.ConfinementClass = now, now, "claude-code", types.CC2
	r.SPIFFEID, r.RunnerTarget = "spiffe://wardyn.test/agent-run/"+r.ID.String(), "docker"
	if _, err := store.NewPG(l.pool).CreateRun(context.Background(), r); err != nil {
		t.Fatalf("persist the run: %v", err)
	}
	sec, err := secretspg.New(l.pool, l.id)
	if err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	m := maskmanifest.New(l.pool, sec.SubjectKeys(), reg)
	fx.srv.cfg.MaskRegistry, fx.srv.cfg.MaskManifests = reg, m
	return &maskedADOLane{adoPATFixture: fx, l: l, reg: reg, m: m}
}

// restarted is a second wardynd's view of the same database.
func (ml *maskedADOLane) restarted(t *testing.T) (*secretmask.Registry, *maskmanifest.Manifests) {
	t.Helper()
	sec, err := secretspg.New(ml.l.pool, ml.l.id)
	if err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	return reg, maskmanifest.New(ml.l.pool, sec.SubjectKeys(), reg)
}

func maskedByRegistry(reg *secretmask.Registry, run types.AgentRun, value string) bool {
	return strings.Contains(string(reg.Masker(run.ID).Mask([]byte("x "+value+" y"))), "<secret-hidden>")
}

// The run token's three renderings are on the manifest before the token is
// returned, at dispatch and for every token minted after it (a renewal), so a
// restarted or second wardynd masks each of them.
func TestMaskManifest_RunTokenRenderingsSurviveARestart(t *testing.T) {
	ml := newMaskedADOLane(t)
	ctx := context.Background()
	if err := ml.m.Start(ctx, ml.run.ID, ml.subject); err != nil {
		t.Fatal(err)
	}
	if !ml.dispatch(t) {
		t.Fatalf("dispatch refused: %q", ml.st.hint)
	}
	if err := ml.m.Complete(ctx, ml.run.ID); err != nil {
		t.Fatal(err)
	}
	first := ml.ok(t, "dev.azure.com", nil)
	ml.setClock(time.UnixMilli(first.ExpiresAt).Add(-adoRunPATRenewWindow + time.Minute))
	ml.ok(t, "dev.azure.com", nil) // the renewal: a second token
	if len(ml.pats.tokens) != 2 {
		t.Fatalf("tokens minted = %d, want the dispatch token and one renewal", len(ml.pats.tokens))
	}

	reg, m := ml.restarted(t)
	if !m.Covered(ctx, ml.run.ID) {
		t.Fatal("a restarted replica does not cover the run")
	}
	for i, tok := range ml.pats.tokens {
		for _, rendering := range []string{tok, base64.StdEncoding.EncodeToString([]byte(":" + tok)), adoRunPATValue(tok)} {
			if !maskedByRegistry(reg, ml.run, rendering) {
				t.Errorf("token %d: the rendering %q is not masked after a restart", i, rendering)
			}
		}
	}
}

// A run dispatched before manifests existed has none to append to: its token
// still mints (refusing it would break a running run) and the run stays
// honestly uncovered.
func TestMaskManifest_LegacyRunTokenMintsAndStaysUncovered(t *testing.T) {
	ml := newMaskedADOLane(t)
	ctx := context.Background()
	if !ml.dispatch(t) {
		t.Fatalf("a legacy run's token was refused: %q", ml.st.hint)
	}
	if ml.m.Covered(ctx, ml.run.ID) {
		t.Error("a run with no manifest is covered after minting a token")
	}
	_, m := ml.restarted(t)
	if m.Covered(ctx, ml.run.ID) {
		t.Error("a restarted replica covers a legacy run")
	}
}

// A token whose renderings cannot be put on the manifest is never handed out:
// the mint fails and the token is revoked at Azure DevOps.
func TestMaskManifest_FailedAppendFailsTheMintAndRevokesTheToken(t *testing.T) {
	ml := newMaskedADOLane(t)
	ctx := context.Background()
	if err := ml.m.Start(ctx, ml.run.ID, ml.subject); err != nil {
		t.Fatal(err)
	}
	if _, err := ml.m.FenceSubject(ctx, ml.subject); err != nil { // the append now fails
		t.Fatal(err)
	}
	if ml.dispatch(t) {
		t.Fatal("a token whose renderings could not be recorded was handed out")
	}
	ml.pats.mu.Lock()
	minted, revoked := len(ml.pats.creates), len(ml.pats.revoked)
	ml.pats.mu.Unlock()
	if minted != 1 || revoked != 1 {
		t.Errorf("tokens created = %d, revoked = %d; want the one it created revoked", minted, revoked)
	}
	if rows := ml.st.unrevoked(t); len(rows) != 0 {
		t.Errorf("%d token row(s) left live after the failed mint", len(rows))
	}
	if err := ml.srv.maskMintedValue(ctx, ml.run.ID, []byte("a-token-minted-after-the-fence")); !errors.Is(err, maskmanifest.ErrFenced) {
		t.Errorf("maskMintedValue on a fenced manifest = %v, want ErrFenced", err)
	}
}
