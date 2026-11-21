// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The mixed-version job's tests: a released v0.10.1 client or server saving the whole document against a
// database a 0.10.2 server has activated. Each needs the released artifact the hosted job provides and skips
// without it; the in-process half (the type 0.10.1 marshals) is TestSiteConfig_OldWriterKeepsProviderServiceGrants_PG.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// assertGrantSurvives checks that corp-a keeps its grant and secret and mints, that a grant write still
// succeeds, and that a replica started afterwards still mints.
func (f *svcPG) assertGrantSurvives(t *testing.T) {
	t.Helper()
	if f.row("corp-a").OIDC.ServiceGrant == nil {
		t.Fatal("the grant did not survive the released version's save")
	}
	if _, err := f.mint("corp-a"); err != nil {
		t.Fatalf("the next mint after the round trip: %v", err)
	}
	p := f.row("corp-a")
	p.OIDC.ServiceGrant.Scope = "api.write"
	if w := f.put(p); w.Code/100 != 2 {
		t.Fatalf("a grant write after the round trip = %d %s", w.Code, w.Body.String())
	}
	if _, err := f.newReplica().mint("corp-a"); err != nil {
		t.Fatalf("a replica started afterwards: %v", err)
	}
}

func (f *svcPG) seedGrant(t *testing.T) {
	t.Helper()
	if w := f.put(svcPGRow("corp-a", true)); w.Code/100 != 2 {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("seed secret: %d %s", w.Code, w.Body.String())
	}
}

// The published v0.10.1 CLI's `site-config get` and its unchanged `site-config set`, against an activated server.
func TestMixedVersion_ReleasedCLIGetSetKeepsServiceGrant(t *testing.T) {
	cli := os.Getenv("WARDYN_TEST_V0101_CLI")
	if cli == "" {
		t.Skip("WARDYN_TEST_V0101_CLI (the released v0.10.1 wardyn binary) is set by the hosted mixed-version job")
	}
	f := newSvcPG(t, true)
	f.seedGrant(t)
	ts := httptest.NewServer(f.srv.Handler())
	defer ts.Close()
	file := filepath.Join(t.TempDir(), "site.json")
	run := func(args ...string) []byte {
		cmd := exec.Command(cli, append(args, "--url", ts.URL, "--token", adminToken)...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, errb.String())
		}
		return out.Bytes()
	}
	if err := os.WriteFile(file, run("site-config", "get"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("site-config", "set", file)
	f.assertGrantSurvives(t)
}

// A running v0.10.1 wardynd on the same database saves the whole document through its own API.
func TestMixedVersion_OldServerSaveKeepsServiceGrant(t *testing.T) {
	base, token := os.Getenv("WARDYN_TEST_V0101_SERVER_URL"), os.Getenv("WARDYN_TEST_V0101_SERVER_TOKEN")
	if base == "" || token == "" {
		t.Skip("WARDYN_TEST_V0101_SERVER_URL and _TOKEN (a v0.10.1 wardynd on this database) are set by the hosted mixed-version job")
	}
	f := newSvcPG(t, true)
	f.seedGrant(t)
	do := func(method string, body io.Reader) []byte {
		req, err := http.NewRequest(method, base+"/api/v1/site-config", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s site-config on the old server = %d %s", method, resp.StatusCode, raw)
		}
		return raw
	}
	doc := do(http.MethodGet, nil)
	do(http.MethodPut, bytes.NewReader(doc))
	f.assertGrantSurvives(t)
}
