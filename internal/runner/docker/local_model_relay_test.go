// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const laptopModelURL = "http://host.docker.internal:11434"

// laptopModelSpec is a run on a laptop local model as placement's copy sends it to a runner.
func laptopModelSpec(t *testing.T, allow bool) runner.SandboxSpec {
	spec := testSpec()
	spec.ProxyConfig.ControlPlaneURL = "https://wardynd.internal:8443"
	caPEM, _ := testCAPEM(t)
	spec.ProxyConfig.ControlPlaneCAPEM = string(caPEM)
	spec.ProxyConfig.LLMUpstreams = map[string]string{"api.anthropic.com": laptopModelURL}
	spec.ProxyConfig.LocalModels = map[string]types.LocalModelSettings{laptopModelURL: {Models: []string{"qwen2.5:3b"}, AllowOnLaptopRunners: allow}}
	return spec
}

func runnerLocalModelsOf(t *testing.T, cfg []byte) (out []string, set bool) {
	t.Helper()
	var c map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	raw, set := c[runnerLocalModelsKey]
	if set {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out, set
}

// On a runner, a laptop local model's port is relayed before its proxy starts, and the proxy is
// told which server the relay serves (runner_local_models). Unflagged, nothing is relayed and the
// proxy's config carries no such key, as before the flag.
func TestLaptopLocalModelRelayedBeforeTheProxyStarts(t *testing.T) {
	for _, allow := range []bool{true, false} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newTestDriver(f)
		var ports []uint16
		d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
		d.cfg.LocalModelRelay = func(p uint16) error { ports = append(ports, p); return nil }
		spec := laptopModelSpec(t, allow)
		if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
			t.Fatalf("allow %v: CreateSandbox: %v", allow, err)
		}
		got, set := runnerLocalModelsOf(t, f.stdinOf(proxyContainerName(spec.RunID)))
		if allow && (!slices.Equal(ports, []uint16{11434}) || !slices.Equal(got, []string{laptopModelURL})) {
			t.Errorf("flag on: relayed ports %v, runner_local_models %v", ports, got)
		}
		if !allow && (len(ports) != 0 || set) {
			t.Errorf("flag off: relayed ports %v, runner_local_models set %v", ports, set)
		}
	}
}

// A runner whose relay cannot serve the port, or has no local model relay, refuses the create
// before the proxy starts.
func TestLaptopLocalModelRefusedWithoutTheRelay(t *testing.T) {
	for name, relay := range map[string]func(uint16) error{
		"no relay":      nil,
		"relay refused": func(uint16) error { return errors.New("injected: port in use") },
	} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newTestDriver(f)
		d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
		d.cfg.LocalModelRelay = relay
		spec := laptopModelSpec(t, true)
		if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "local model") {
			t.Errorf("%s: CreateSandbox = %v, want the local model refusal", name, err)
		}
		if len(f.stdinOf(proxyContainerName(spec.RunID))) != 0 {
			t.Errorf("%s: a proxy was started", name)
		}
	}
}

// Only a runner's driver sets runner_local_models: off a runner the flag relays nothing and a key the
// control plane wrote is taken off; on a runner a flagged server off host.docker.internal is refused.
func TestRunnerLocalModelsSetOnlyByARunner(t *testing.T) {
	d := newTestDriver(newFakeDocker())
	d.cfg.LocalModelRelay = func(uint16) error { t.Error("relayed off a runner"); return nil }
	cfg := []byte(`{"local_models":{"` + laptopModelURL + `":{"models":["m"],"allow_on_laptop_runners":true}},"runner_local_models":["` + laptopModelURL + `"]}`)
	out, err := d.withRunnerLocalModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := runnerLocalModelsOf(t, out); set {
		t.Errorf("the organisation's driver kept runner_local_models: %s", out)
	}
	d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
	if _, err := d.withRunnerLocalModels([]byte(`{"local_models":{"http://10.0.4.7:11434":{"models":["m"],"allow_on_laptop_runners":true}}}`)); err == nil || !strings.Contains(err.Error(), "must be on host.docker.internal") {
		t.Errorf("a flagged node address on a runner = %v", err)
	}
}
