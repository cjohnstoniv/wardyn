// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestHealthzOrgFederation_GoldenWhenOff pins the anonymous /healthz body of a
// daemon with no WARDYN_ORG_URL byte for byte: hybrid enrolment must not change
// a single byte a non-hybrid deployment already serves. The build's version is
// the one field replaced before comparing, so a release bump does not rot it.
// Regenerate with: WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run TestHealthzOrgFederation
func TestHealthzOrgFederation_GoldenWhenOff(t *testing.T) {
	const path = "testdata/healthz_no_org_golden.json"
	w := do(t, newHarness(t).srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d", w.Code)
	}
	got := bytes.ReplaceAll(w.Body.Bytes(), []byte(`"version":"`+version.Version+`"`), []byte(`"version":"<version>"`))
	if goldenUpdate() {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("/healthz with no org URL changed:\n--- golden ---\n%s\n--- now ---\n%s", want, got)
	}
}

// TestHealthzOrgFederation_BlockNeverNamesTheDevice pins what a hybrid laptop's
// anonymous /healthz adds: lag and enrolled, and neither the device id nor
// anything that could carry the org URL.
func TestHealthzOrgFederation_BlockNeverNamesTheDevice(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	st := federation.Status{DeviceID: id, AckedSeq: 40, HeadSeq: 52, LastError: "dial tcp org.example.test:443: refused"}
	h.srv.cfg.OrgFederation = func() federation.Status { return st }
	body := do(t, h.srv, http.MethodGet, "/healthz", "", "").Body.String()
	var got struct {
		OrgFederation map[string]any `json:"org_federation"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.OrgFederation) != 2 || got.OrgFederation["enrolled"] != true || got.OrgFederation["lag"] != float64(12) {
		t.Fatalf("org_federation = %v", got.OrgFederation)
	}
	for _, leak := range []string{id.String(), "org.example.test"} {
		if strings.Contains(body, leak) {
			t.Errorf("/healthz discloses %q: %s", leak, body)
		}
	}
	if m := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "").Body.String(); !strings.Contains(m, "\nwardyn_org_federation_lag 12\n") {
		t.Errorf("/metrics lacks the lag gauge:\n%s", m)
	}
	st.Revoked = true
	body = do(t, h.srv, http.MethodGet, "/healthz", "", "").Body.String()
	if !strings.Contains(body, `"org_federation":{"enrolled":false,"lag":12}`) {
		t.Errorf("revoked device still reads enrolled: %s", body)
	}
}

// TestCreateRunRefusedWhenRevoked drives the real POST /runs to its store
// write: enrolled, the run is created; revoked, the answer is 503 naming
// re-enrolment and no row is written.
func TestCreateRunRefusedWhenRevoked(t *testing.T) {
	h := newHarness(t)
	st := &runWarnStore{capStore: &capStore{}}
	cfg := baseTestConfig(h, st)
	cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	revoked := false
	cfg.OrgFederation = func() federation.Status { return federation.Status{Revoked: revoked} }
	srv := New(cfg)
	const body = `{"agent":"claude-code","task":"t"}`
	if w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body); w.Code != http.StatusCreated || st.created.ID == uuid.Nil {
		t.Fatalf("enrolled: code = %d, want 201 with a row: %s", w.Code, w.Body)
	}
	st.created = types.AgentRun{}
	revoked = true
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "re-enrolled") {
		t.Fatalf("revoked: code = %d body = %s, want 503 naming re-enrolment", w.Code, w.Body)
	}
	if st.created.ID != uuid.Nil {
		t.Fatal("revoked: a run row was written")
	}
}

// TestCreateRunGateRefusesBeforeTheStore: the shared gate answers errOrgRevoked
// without touching the store, and writeServerError — where every launcher sends
// a creation failure — turns it into the 503 naming re-enrolment.
func TestCreateRunGateRefusesBeforeTheStore(t *testing.T) {
	st := &runWarnStore{capStore: &capStore{}}
	srv := New(Config{Store: st, OrgFederation: func() federation.Status { return federation.Status{Revoked: true} }})
	if _, err := srv.createRun(context.Background(), types.AgentRun{ID: uuid.New()}); !errors.Is(err, errOrgRevoked) || st.created.ID != uuid.Nil {
		t.Fatalf("err = %v, row written = %v", err, st.created.ID != uuid.Nil)
	}
	w := httptest.NewRecorder()
	writeServerError(w, httptest.NewRequest(http.MethodPost, "/", nil), "launch scan run", fmt.Errorf("create scan run: %w", errOrgRevoked))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "re-enrolled") {
		t.Fatalf("writeServerError: code = %d body = %s", w.Code, w.Body)
	}
}

// TestRunCreationRoutesThroughTheRevocationGate enumerates every run-creating
// path in this package and fails if one skips createRun's revocation gate: a
// method named CreateRun — on any receiver — may be called only from createRun, each launcher below must
// call createRun, and a sandbox is created only by the dispatch chain, which
// runs for a row createRun wrote. A new launcher that calls the store directly,
// or a listed one that stops calling the gate, fails here.
func TestRunCreationRoutesThroughTheRevocationGate(t *testing.T) {
	wantGated := []string{
		"harnesscred.go:launchHarnessLoginRun",
		"runs.go:handleCreateRun",
		"site_config_probe.go:runSiteConfigProbe",
		"source_scan.go:launchSourceScanRun",
		"workspace_run_launch.go:launchRecordRun",
	}
	// Each dispatches a row one of the gated launchers above just created.
	wantDispatchers := []string{
		"harnesscred_launch.go:finishHarnessLoginLaunch",
		// 0.7.10 moved handleCreateRun's blocking half out of runs.go; the
		// gated createRun call it dispatches for stayed in handleCreateRun.
		"runs_create_launch.go:finishCreateRunLaunch",
		"site_config_probe.go:runSiteConfigProbe",
		"workspace_run_launch.go:dispatchAndSettle",
	}
	var gated, direct, sandboxes, dispatchers []string
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			where := name + ":" + fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// By method name, whatever the receiver: an aliased store
				// (st := s.cfg.Store; st.CreateRun) or one passed in as a
				// parameter must not slip past.
				switch sel.Sel.Name {
				case "CreateRun":
					direct = append(direct, where)
				case "createRun":
					gated = append(gated, where)
				case "CreateSandbox":
					sandboxes = append(sandboxes, where)
				case "dispatchRun":
					dispatchers = append(dispatchers, where)
				}
				return true
			})
		}
	}
	slices.Sort(gated)
	slices.Sort(dispatchers)
	if !slices.Equal(direct, []string{"org_revocation.go:createRun"}) {
		t.Errorf("a CreateRun method is called from %v; only org_revocation.go:createRun may call one — route the launcher through s.createRun", direct)
	}
	if !slices.Equal(gated, wantGated) {
		t.Errorf("launchers calling the revocation gate = %v, want %v", gated, wantGated)
	}
	if !slices.Equal(sandboxes, []string{"runs_dispatch.go:dispatchRun"}) {
		t.Errorf("a CreateSandbox method is called from %v; a sandbox must only ever be created by dispatchRun, for a run createRun wrote", sandboxes)
	}
	if !slices.Equal(dispatchers, wantDispatchers) {
		t.Errorf("dispatchRun callers = %v, want %v — a new dispatch path must dispatch a row createRun wrote, then be listed here", dispatchers, wantDispatchers)
	}
}
