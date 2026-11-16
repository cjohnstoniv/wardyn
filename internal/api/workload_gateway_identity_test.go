// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// A workload's run's gateway token names the run under the workload in act.sub, the run token's own ID,
// and the workload in wardyn_workload; the first-mint row names the workload too.
func TestGatewayMintNamesAWorkloadsRun(t *testing.T) {
	f := newGatewayFixture(t)
	ri, err := f.h.idp.MintWorkloadRunIdentity(context.Background(), f.run.ID, testRunWorkload, "alice@example.com", "", internalAudience, false)
	if err != nil {
		t.Fatal(err)
	}
	f.token = ri.Token
	w := f.mint(t)
	if w.Code != http.StatusOK {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(body.Token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var c gatewayidentity.Claims
	if err := parsed.Claims(testGatewayKeySet(t, f.signer), &c); err != nil {
		t.Fatal(err)
	}
	if c.Issuer != "spiffe://wardyn.local" || c.Workload != testRunWorkload.String() || c.Run != f.run.ID.String() {
		t.Errorf("claims = %+v, want the issuer, the workload and the run", c)
	}
	var raw struct {
		Act struct {
			Sub string `json:"sub"`
		} `json:"act"`
	}
	if err := parsed.UnsafeClaimsWithoutVerification(&raw); err != nil || raw.Act.Sub != ri.SPIFFEID {
		t.Errorf("act.sub = %q, %v; want the run token's %q", raw.Act.Sub, err, ri.SPIFFEID)
	}
	evs := f.mintEvents()
	var data map[string]any
	if len(evs) != 1 || json.Unmarshal(evs[0].Data, &data) != nil || data["workload"] != testRunWorkload.String() {
		t.Fatalf("first-mint rows = %+v, want one naming the workload", evs)
	}
}

// An ad-hoc run's first-mint row carries no workload key.
func TestGatewayMintRowOfAnAdHocRunNamesNoWorkload(t *testing.T) {
	f := newGatewayFixture(t)
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	evs := f.mintEvents()
	var data map[string]any
	if len(evs) != 1 || json.Unmarshal(evs[0].Data, &data) != nil {
		t.Fatalf("first-mint rows = %+v", evs)
	}
	if v, has := data["workload"]; has {
		t.Fatalf("an ad-hoc run's row names workload %v", v)
	}
}

// A run token whose SPIFFE ID names another run than its own is refused, never signed: act.sub and the
// client certificate would name two runs.
func TestSignGatewayTokenRefusesAnIDOfAnotherRun(t *testing.T) {
	f := newGatewayFixture(t)
	dest := gatewayDest{audience: "gw-aud", lifetime: time.Minute, provider: "corp-gw"}
	for name, id := range map[string]string{
		"a workload's other run": testWorkloadRunID(t, uuid.New()),
		"an ad-hoc other run":    "spiffe://wardyn.local/agent-run/" + uuid.New().String(),
		"not a run's":            "spiffe://wardyn.local/workload/alice/web",
	} {
		claims := &identity.Claims{Sub: "alice@example.com", RunID: f.run.ID, SPIFFEID: id}
		if _, _, err := f.srv.signGatewayToken(context.Background(), claims, f.run, dest, ""); err == nil {
			t.Errorf("%s: signed a token for %q", name, id)
		}
	}
}
