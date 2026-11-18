// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A cluster minted_pat answer is a lease, not the PAT's validTo: the proxy asks
// again within storedKeyTTL, the asks mint nothing, and renewal still mints
// once near validTo.
func TestMintedPAT_ClusterAnswerExpiresWithinStoredKeyTTL(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	validTo := fx.validTo(t, first.JTI)
	if limit := fx.clock().Add(storedKeyTTL); time.UnixMilli(first.ExpiresAt).After(limit) || !validTo.After(limit) {
		t.Fatalf("expiry %v, token validTo %v: want the expiry at most %v and the token outliving it", time.UnixMilli(first.ExpiresAt), validTo, limit)
	}

	for range 12 {
		fx.advance(5 * time.Minute)
		got := fx.ok(t, "dev.azure.com", nil)
		if limit := fx.clock().Add(storedKeyTTL); got.JTI != first.JTI || got.ExpiresAt > limit.UnixMilli() {
			t.Fatalf("at %v: JTI %s expires %v, want the cached token leased to at most %v", fx.clock(), got.JTI, time.UnixMilli(got.ExpiresAt), limit)
		}
	}
	if n := fx.pats.createCount(); n != 1 {
		t.Fatalf("creates after an hour of re-resolves = %d, want 1", n)
	}
	if got := fx.auditReasons(adoPATAuditMint); !slices.Equal(got, []string{adoPATMintDispatch}) {
		t.Fatalf("ado_pat.mint reasons = %v, want [dispatch]", got)
	}

	fx.setClock(validTo.Add(-adoRunPATRenewWindow + time.Minute))
	renewed := fx.ok(t, "dev.azure.com", nil)
	if renewed.JTI == first.JTI || fx.pats.createCount() != 2 {
		t.Fatalf("near validTo: JTI %s creates %d, want one renewal", renewed.JTI, fx.pats.createCount())
	}
	if again := fx.ok(t, "dev.azure.com", nil); again.JTI != renewed.JTI || fx.pats.createCount() != 2 {
		t.Fatalf("after the renewal: JTI %s creates %d, want the renewed token and no further mint", again.JTI, fx.pats.createCount())
	}
	if got := fx.auditReasons(adoPATAuditMint); !slices.Equal(got, []string{adoPATMintDispatch, adoPATMintRenewal}) {
		t.Fatalf("ado_pat.mint reasons = %v, want [dispatch renewal]", got)
	}
}

// A write to the row, or a cap, reaches a cluster run at the resolve its proxy
// makes one lease after its last: the run is refused and its token revoked.
func TestMintedPAT_ClusterRowWriteReachesTheRunWithinALease(t *testing.T) {
	for name, tc := range map[string]struct {
		write     func(fx *adoPATFixture)
		reason    string
		abandoned bool // the revoke is abandoned: the row is gone, so nothing is revoked
	}{
		"row disabled": {func(fx *adoPATFixture) {
			live := fx.srv.cfg.ADOEntra
			fx.srv.cfg.ADOEntraByRow = func(_ context.Context, id string) (ADOEntraConfig, bool, error) {
				cfg, _, err := live(context.Background())
				return cfg, id == cfg.RowID, err
			}
			fx.srv.cfg.ADOEntra = func(ctx context.Context) (ADOEntraConfig, bool, error) {
				if fx.st.site.WorkspaceProviders.Git[0].Disabled {
					return ADOEntraConfig{}, false, nil
				}
				return live(ctx)
			}
			fx.st.site.WorkspaceProviders.Git[0].Disabled = true
		}, reasonScopeChanged, false},
		"row deleted": {func(fx *adoPATFixture) {
			live := fx.srv.cfg.ADOEntra
			fx.srv.cfg.ADOEntraByRow = func(ctx context.Context, id string) (ADOEntraConfig, bool, error) {
				cfg, ok, err := live(ctx)
				return cfg, ok && id == cfg.RowID, err
			}
			fx.st.site.WorkspaceProviders.Git = nil
		}, reasonScopeChanged, true},
		"ceiling narrowed": {func(fx *adoPATFixture) {
			fx.st.site.WorkspaceProviders.Git[0].Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapCodeRead}
		}, reasonScopeChanged, false},
		"token mode switched": {func(fx *adoPATFixture) {
			fx.st.site.WorkspaceProviders.Git[0].Entra.TokenMode = types.ADOTokenModeBearer
		}, reasonScopeChanged, false},
		"boundary cap without a held capability": {func(fx *adoPATFixture) {
			fx.srv.cfg.Store = &vcsPATStore{adoPATStore: fx.st, b: capBoundary(adoRead)}
		}, reasonCapabilityCapped, false},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newADOPATFixture(t)
			first := fx.ok(t, "dev.azure.com", nil)
			if limit := fx.clock().Add(storedKeyTTL); first.ExpiresAt > limit.UnixMilli() {
				t.Fatalf("the answer expires %v, want at most %v: the proxy must ask again within a lease", time.UnixMilli(first.ExpiresAt), limit)
			}
			tc.write(fx)
			fx.advance(storedKeyTTL - 5*time.Minute)
			w, _ := fx.resolve(t, "dev.azure.com", nil)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), tc.reason) {
				t.Fatalf("resolve one lease later: status %d body %s, want 403 %s", w.Code, w.Body.String(), tc.reason)
			}
			wantRevoked := []string{first.JTI}
			if tc.abandoned {
				wantRevoked = nil
			}
			if got := fx.pats.revokedIDs(); !slices.Equal(got, wantRevoked) {
				t.Fatalf("revoked %v, want %v", got, wantRevoked)
			}
			if live := fx.st.unrevoked(t); len(live) != 0 {
				t.Fatalf("live rows = %+v, want none", live)
			}
			if last := fx.st.marks[len(fx.st.marks)-1]; last.reason != adoPATRevokeDrift {
				t.Fatalf("closed with %+v, want reason %q", last, adoPATRevokeDrift)
			} else if tc.abandoned && last.lastError == "" {
				t.Fatalf("closed with %+v, want the abandoned revoke's error recorded", last)
			}
		})
	}
}
