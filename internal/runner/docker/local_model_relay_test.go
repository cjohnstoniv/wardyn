// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// fakeModelRelay records what the driver asks of wardyn-runnerd's local model relay.
type fakeModelRelay struct {
	open    map[uuid.UUID][]netip.Addr // runs admitted now, with their proxy's addresses
	ports   []uint16
	closed  []uuid.UUID
	openErr error
	t       *testing.T // set: any open fails the test
}

func (r *fakeModelRelay) OpenLocalModel(runID uuid.UUID, ports []uint16, peers []netip.Addr) error {
	if r.t != nil {
		r.t.Error("relayed off a runner")
	}
	if r.openErr != nil {
		return r.openErr
	}
	if r.open == nil {
		r.open = map[uuid.UUID][]netip.Addr{}
	}
	r.open[runID], r.ports = peers, ports
	return nil
}

func (r *fakeModelRelay) CloseLocalModel(runID uuid.UUID) {
	delete(r.open, runID)
	r.closed = append(r.closed, runID)
}

// On a runner, a laptop local model's port is served once its proxy runs, to that proxy's own
// addresses alone, and the proxy is told which server the relay serves (runner_local_models). The
// run's end, by EndSandbox or StopSandbox, ends the admission. Unflagged, nothing is served and the
// proxy's config carries no such key, as before the flag.
func TestLaptopLocalModelRelayedOnlyToItsRunningProxy(t *testing.T) {
	for _, allow := range []bool{true, false} {
		for _, end := range []string{"EndSandbox", "StopSandbox"} {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newTestDriver(f)
			relay := &fakeModelRelay{}
			d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
			d.cfg.LocalModelRelay = relay
			spec := laptopModelSpec(t, allow)
			sb, err := d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Fatalf("allow %v: CreateSandbox: %v", allow, err)
			}
			got, set := runnerLocalModelsOf(t, f.stdinOf(proxyContainerName(spec.RunID)))
			peers := relay.open[spec.RunID]
			slices.SortFunc(peers, netip.Addr.Compare)
			want := []netip.Addr{netip.MustParseAddr("10.88.0.2"), netip.MustParseAddr(fakeJoinedNetIP)}
			if allow && (!slices.Equal(relay.ports, []uint16{11434}) || !slices.Equal(peers, want) || !slices.Equal(got, []string{laptopModelURL})) {
				t.Errorf("flag on: served ports %v to %v, want 11434 to the proxy's %v; runner_local_models %v", relay.ports, peers, want, got)
			}
			if !allow && (len(relay.open) != 0 || set) {
				t.Errorf("flag off: served %v, runner_local_models set %v", relay.open, set)
			}
			if end == "EndSandbox" {
				err = d.EndSandbox(context.Background(), sb.Ref)
			} else {
				err = d.StopSandbox(context.Background(), sb.Ref)
			}
			if err != nil {
				t.Fatalf("%s: %v", end, err)
			}
			if _, still := relay.open[spec.RunID]; still || !slices.Contains(relay.closed, spec.RunID) {
				t.Errorf("allow %v: the relay still admits run %s after %s removed its proxy", allow, spec.RunID, end)
			}
		}
	}
}

// A runner with no local model relay refuses the create before the proxy starts; one whose relay
// cannot serve the port refuses it and removes the started proxy.
func TestLaptopLocalModelRefusedWithoutTheRelay(t *testing.T) {
	for name, relay := range map[string]LocalModelRelay{
		"no relay":      nil,
		"relay refused": &fakeModelRelay{openErr: errors.New("injected: port in use")},
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
		if c := f.containers[proxyContainerName(spec.RunID)]; c != nil && !c.removed {
			t.Errorf("%s: the proxy was left running", name)
		}
	}
}

// Only a runner's driver sets runner_local_models: off a runner the flag relays nothing and a key the
// control plane wrote is taken off; on a runner a flagged server off host.docker.internal is refused.
func TestRunnerLocalModelsSetOnlyByARunner(t *testing.T) {
	d := newTestDriver(newFakeDocker())
	d.cfg.LocalModelRelay = &fakeModelRelay{t: t}
	cfg := []byte(`{"local_models":{"` + laptopModelURL + `":{"models":["m"],"allow_on_laptop_runners":true}},"runner_local_models":["` + laptopModelURL + `"]}`)
	out, ports, err := d.withRunnerLocalModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := runnerLocalModelsOf(t, out); set || len(ports) != 0 {
		t.Errorf("the organisation's driver kept runner_local_models or relays %v: %s", ports, out)
	}
	d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "host-gateway", nil }
	if _, _, err := d.withRunnerLocalModels([]byte(`{"local_models":{"http://10.0.4.7:11434":{"models":["m"],"allow_on_laptop_runners":true}}}`)); err == nil || !strings.Contains(err.Error(), "must be on host.docker.internal") {
		t.Errorf("a flagged node address on a runner = %v", err)
	}
}
