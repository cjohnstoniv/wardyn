// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// The claim at the end of setup trusts what registration trusted: an organisation signed only by the
// --org-ca-file CA is registered and then claimed.
func TestSetupClaimsThroughTheOrgCA(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("WARDYN_ORG_CA_FILE", "")
	t.Setenv("WARDYN_TOKEN", "wdn_secret")
	f := newSetupFixture(t, "linux")
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.org = srv.URL
	ca := writeFile(t, filepath.Join(t.TempDir(), "org-ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, orgCAFile: ca, claim: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.count("POST /api/v1/runners/register") != 1 || f.count("POST /api/v1/me/runners/"+f.id.String()+"/claim") != 1 {
		t.Fatalf("registered %d, claimed %d; want both once, through the org CA:\n%s",
			f.count("POST /api/v1/runners/register"), f.count("POST /api/v1/me/runners/"+f.id.String()+"/claim"), out)
	}
}
