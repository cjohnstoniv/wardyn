// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The remote-runner internal_hosts pin: two names answering in CGNAT
// (100.64.0.0/10), one under the site's declared suffix and one not. Both are
// on the run's allowlist, so only the private-IP guard decides. The proxy
// package's TestInternalHost_DispatchedConfigVerdicts uses the same names.
const (
	internalHostDeclared   = "svc.corp.internal"
	internalHostUndeclared = "svc.other.internal"
)

func internalHostsSite() types.SiteConfig {
	return types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}
}

// siteConfigDispatchStore answers the site config read at dispatch and at
// revive from *sc, so a test can remove a declaration between the two.
type siteConfigDispatchStore struct {
	localDispatchStore
	sc *types.SiteConfig
}

func (s siteConfigDispatchStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return *s.sc, nil
}

// DeleteRunProxyConfig keeps the row: dispatch drops a run's config when the
// run fails, and here it fails at the runner lease (see dispatchInternalHostsRun).
func (siteConfigDispatchStore) DeleteRunProxyConfig(context.Context, uuid.UUID) error { return nil }

// dispatchInternalHostsRun runs the organisation's dispatch (dispatchRun) for a
// local-placement run bound to runnerID under *sc and returns the runner spec
// its proxy config makes. Dispatch renders internal_hosts from the site config
// (runs_dispatch.go), classifies the spec for the laptop and stores the
// classified proxy config (classifyLocalDispatch), then leases the run to its
// runner, which needs Postgres (leaseRunnerExecution): without it dispatch
// stops there, before CreateSandbox. The stored row is the config the runner
// would have been handed (kept past the failure, see DeleteRunProxyConfig), and
// the one revive rebuilds from; localStoredSpec is
// revive's own conversion back to a spec. The control-plane pair is the
// server's (s.cfg), which dispatch and revive both read.
func dispatchInternalHostsRun(t *testing.T, sc *types.SiteConfig, runnerID uuid.UUID, image, cpURL, cpCAPEM string) (*Server, types.AgentRun, runner.SandboxSpec) {
	t.Helper()
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	run.RunnerID = &runnerID
	ls.dispatchTestStore.run, ls.dispatchTestStore.state = run, types.RunPending
	srv.cfg.Store = siteConfigDispatchStore{localDispatchStore: ls, sc: sc}
	srv.cfg.ControlPlaneURL, srv.cfg.ControlPlaneCAPEM = cpURL, cpCAPEM
	ctx := context.Background()
	srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: image, Interactive: true,
		Policy: types.RunPolicySpec{AllowedDomains: []string{internalHostDeclared, internalHostUndeclared}, MinConfinementClass: types.CC1},
	})
	if hint := ls.dispatchTestStore.FailureHint(); !strings.Contains(hint, "lease the run to its runner") {
		t.Fatalf("dispatch did not reach the runner lease: %s %q", ls.dispatchTestStore.State(), hint)
	}
	raw, err := srv.loadRunProxyConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	spec := localStoredSpec(*cfg)
	if spec.ProxyConfig.Policy.LLMInspection != nil {
		// The stored row equals the runner's spec only without an inspection corpus (withInspectionCorpus).
		t.Fatal("this pin assumes a policy with no inspection corpus")
	}
	spec.Image, spec.ConfinementClass, spec.RunnerID = image, types.CC1, runnerID.String()
	return srv, run, spec
}

// dispatchedInternalHostsGolden is the wire tier's hand-off: the config a
// runner's proxy loads for the run below, which the proxy package's
// TestInternalHost_DispatchedConfigVerdicts boots and asserts verdicts on.
const dispatchedInternalHostsGolden = "../egress/proxy/testdata/dispatched_internal_hosts.json"

// TestInternalHostsDispatchWireConfig is the wire tier of the remote-runner
// pin: the spec local dispatch produced crosses the runner wire encoding
// (runnerwire CreateSandboxArgs), is rendered as a runner renders it
// (runner.BuildProxyConfig) and loads in the proxy (LoadConfigBytes) with the
// site's declaration intact. The rendered bytes, run id fixed, are the golden
// the proxy package boots; regenerate it with
// WARDYN_UPDATE_GOLDEN=1 go test ./internal/api -run TestInternalHostsDispatchWireConfig
func TestInternalHostsDispatchWireConfig(t *testing.T) {
	sc := internalHostsSite()
	_, _, spec := dispatchInternalHostsRun(t, &sc, uuid.New(), "busybox:latest", "http://127.0.0.1:9", "")

	data, err := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	var onRunner runnerwire.CreateSandboxArgs
	if err := runnerwire.Unmarshal(data, &onRunner); err != nil {
		t.Fatal(err)
	}
	raw, err := runner.BuildProxyConfig(onRunner.Spec.RunID, onRunner.Spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.InternalHosts) != 1 || cfg.InternalHosts[0].HostSuffix != "corp.internal" {
		t.Fatalf("the runner's proxy config carries internal_hosts %+v, want the site's one declaration", cfg.InternalHosts)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["run_id"] = "00000000-0000-4000-8000-000000000001"
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("WARDYN_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(dispatchedInternalHostsGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(dispatchedInternalHostsGolden)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is not what local dispatch renders for a runner (%v); regenerate with WARDYN_UPDATE_GOLDEN=1 go test ./internal/api -run TestInternalHostsDispatchWireConfig\n%s",
			dispatchedInternalHostsGolden, err, got)
	}
}
