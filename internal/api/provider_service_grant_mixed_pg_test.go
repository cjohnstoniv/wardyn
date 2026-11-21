// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The mixed-version job's tests: a released v0.10.1 client or server saving the whole document against a
// database a 0.10.2 server has activated. Each needs the released artifact the hosted job provides and skips
// without it; the in-process half (the type 0.10.1 marshals) is TestSiteConfig_OldWriterKeepsProviderServiceGrants_PG.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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

// startReleasedServer starts the released v0.10.1 wardynd at bin on this fixture's own database and its sealed
// secret store's key, as the hosted job's old replica: the schema is ahead of it, so it runs with
// WARDYN_ALLOW_UNKNOWN_MIGRATIONS. It returns the
// server's base URL and admin token once /healthz answers, and stops the process at cleanup.
func (f *svcPG) startReleasedServer(t *testing.T, bin string) (string, string) {
	t.Helper()
	if f.ageKey == "" {
		t.Fatal("the released server needs the fixture's sealed secret store (newSvcPGSealed)")
	}
	ageKey := f.ageKey
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	listen, internal := port(), port()
	token := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "WARDYN_PG_DSN="+f.pool.Config().ConnString(), "WARDYN_LISTEN="+listen,
		"WARDYN_INTERNAL_LISTEN="+internal, "WARDYN_RUNNER=none", "WARDYN_AGE_KEY="+ageKey, "WARDYN_ADMIN_TOKEN="+token,
		"WARDYN_ALLOW_UNKNOWN_MIGRATIONS=true")
	var logs lockedBuffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start the released wardynd: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		cancel()
		if !svcWaited(exited) {
			t.Error("the released wardynd did not stop")
		}
	})
	base := "http://" + listen
	client := &http.Client{Timeout: 2 * time.Second}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(60 * time.Second)
	for {
		if resp, err := client.Get(base + "/healthz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, token
			}
		}
		select {
		case <-exited:
			t.Fatalf("the released wardynd exited before serving /healthz:\n%s", logs.String())
		case <-deadline:
			t.Fatalf("the released wardynd did not serve /healthz within 60s:\n%s", logs.String())
		case <-tick.C:
		}
	}
}

// A released v0.10.1 wardynd and this server, both on the one throwaway database: the old server's GET shows a
// marker seeded through this server, an edit of a field 0.10.1 knows is saved through the old server's whole-
// document PUT, and this server then reads that edit with the grant still attached, still minting.
func TestMixedVersion_OldServerSaveKeepsServiceGrant(t *testing.T) {
	bin := os.Getenv("WARDYN_TEST_V0101_WARDYND")
	if bin == "" {
		t.Skip("WARDYN_TEST_V0101_WARDYND (the released v0.10.1 wardynd binary) is set by the hosted mixed-version job")
	}
	f := newSvcPGSealed(t, true)
	seeded, edited := "seed-"+uuid.NewString(), "edit-"+uuid.NewString()
	row := svcPGRow("corp-a", true)
	row.Name = seeded
	if w := f.put(row); w.Code/100 != 2 {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("seed secret: %d %s", w.Code, w.Body.String())
	}
	base, token := f.startReleasedServer(t, bin)
	do := func(method string, body io.Reader) []byte {
		req, err := http.NewRequest(method, base+"/api/v1/site-config", body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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
	if !bytes.Contains(doc, []byte(seeded)) {
		t.Fatalf("the old server's document does not show the marker seeded on this database: %s", doc)
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	mp, _ := m["model_providers"].(map[string]any)
	rows, _ := mp["providers"].([]any)
	renamed := false
	for _, r := range rows {
		if p, ok := r.(map[string]any); ok && p["id"] == "corp-a" {
			p["name"], renamed = edited, true
		}
	}
	if !renamed {
		t.Fatalf("the old server's document has no corp-a row: %s", doc)
	}
	do(http.MethodPut, bytes.NewReader(mustJSON(m)))
	// The released save purges provider credentials before it writes: the grant's record must survive it.
	raw, err := f.sealed.Get(context.Background(), f.secretNameOf("corp-a"))
	if rec, ok := parseServiceGrantRecord(raw); err != nil || !ok || rec.ClientSecret != svcClientSecret {
		t.Fatalf("the grant's record after the old server's save = %v, %v; want it kept", ok, err)
	}
	if got := f.row("corp-a"); got.Name != edited || got.OIDC == nil || got.OIDC.ServiceGrant == nil {
		t.Fatalf("this server reads corp-a as %q with grant %v, want the old server's edit %q and the grant kept", got.Name, got.OIDC, edited)
	}
	if w := f.do(http.MethodGet, "/model-providers", f.admin, nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), edited) || !strings.Contains(w.Body.String(), serviceGrantSecretName(f.uid("corp-a"))) {
		t.Fatalf("this server's GET /model-providers = %d %s, want the edit and the grant's secret reference", w.Code, w.Body.String())
	}
	f.assertGrantSurvives(t)
}
