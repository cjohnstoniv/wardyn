// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// An admin whose request is gone before the canary create still gets the
// canary created and revoked: the create runs on a context that outlives the
// request, like the revoke (#2136).
func TestOrgCheck_CanaryCreateSurvivesTheRequestGone(t *testing.T) {
	mf := newMintFixture(t)
	mf.connect(t)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	var res adoOrgCheckResult
	refusal, err := mf.srv.adoOrgCheckCanary(gone, mf.cfg, mf.subject, "contoso", time.Now().Add(adoOrgCheckCanaryLife), &res)
	if err != nil || refusal != nil {
		t.Fatalf("canary = refusal %v, err %v; want it created", refusal, err)
	}
	if c, r := mf.ado.Count(adofake.EndpointPatsCreate), mf.ado.Count(adofake.EndpointPatsRevoke); c != 1 || r != 1 || len(res.Unrevoked) != 0 {
		t.Fatalf("creates = %d revokes = %d unrevoked = %v, want one canary created and revoked", c, r, res.Unrevoked)
	}
}

// maskPutFails fails only the registration of a minted token's mask.
type maskPutFails struct{ secretmask.Backend }

func (maskPutFails) GlobalGeneration(context.Context) (int64, error) { return 0, nil }

func (maskPutFails) PutGlobal(_ int64, _, name string, _ []secretmask.GlobalPut, _ bool, _ time.Time) error {
	if strings.HasPrefix(name, "ado-pat-") {
		return errors.New("mask store down")
	}
	return nil
}

// A canary created but not recordable for masking is not handed out, and it is
// revoked rather than left live in the admin's account for 364 days (#2136).
func TestOrgCheck_CanaryRevokedWhenMaskingFails(t *testing.T) {
	mf := newMintFixture(t)
	mf.connect(t)
	mf.srv.cfg.MaskRegistry.SetBackend(maskPutFails{})
	var res adoOrgCheckResult
	_, err := mf.srv.adoOrgCheckCanary(context.Background(), mf.cfg, mf.subject, "contoso", time.Now().Add(adoOrgCheckCanaryLife), &res)
	if !errors.Is(err, ErrADOEntraUnavailable) {
		t.Fatalf("err = %v, want ErrADOEntraUnavailable", err)
	}
	if c, r := mf.ado.Count(adofake.EndpointPatsCreate), mf.ado.Count(adofake.EndpointPatsRevoke); c != 1 || r != 1 || len(res.Unrevoked) != 0 {
		t.Fatalf("creates = %d revokes = %d unrevoked = %v, want the created canary revoked", c, r, res.Unrevoked)
	}
}
