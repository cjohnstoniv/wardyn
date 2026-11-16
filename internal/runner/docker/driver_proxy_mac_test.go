// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyEndpoint is the run-network endpoint the fake recorded at the create of runID's current proxy.
func proxyEndpoint(f *fakeDocker, runID uuid.UUID) *network.EndpointSettings {
	if p := f.containers[proxyContainerName(runID)]; p != nil && p.net != nil {
		return p.net.EndpointsConfig[internalNetName(runID)]
	}
	return nil
}

// TestProxyMAC_EveryProxyOfARunTakesTheRunsMAC: the proxy's create, the replace of a running proxy that a live
// egress edit makes, and the revive of a stopped proxy all ask for one MAC address on the run network, the run's
// own, and the last two ask for it beside the address the agent pins. So the agent's cached neighbour entry for
// that address reaches every proxy the run gets. It is a locally administered unicast address, and another run's
// differs.
func TestProxyMAC_EveryProxyOfARunTakesTheRunsMAC(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	ctx := context.Background()
	spec := testSpec()
	runID := spec.RunID
	want := proxyMAC(runID)
	if len(want) != 6 || want[0]&0x03 != 0x02 {
		t.Fatalf("proxyMAC = %s, want six bytes, the first with the locally administered bit set and the multicast bit clear", want)
	}
	if other := proxyMAC(uuid.New()); bytes.Equal(other, want) {
		t.Fatalf("two runs share the MAC %s", want)
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	check := func(step string, pinned bool) {
		t.Helper()
		ep := proxyEndpoint(f, runID)
		if ep == nil || ep.MacAddress.String() != want.String() {
			t.Fatalf("after %s: the proxy's run-network endpoint = %+v, want MAC %s", step, ep, want)
		}
		if pinned && (ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address.String() != "10.88.0.2") {
			t.Errorf("after %s: the proxy's run-network endpoint = %+v, want it pinned to the agent's wardyn-proxy address 10.88.0.2", step, ep)
		}
	}
	check("the create", false)
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxy of the running proxy: %v", err)
	}
	check("a replace of the running proxy", true)
	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "again")); err != nil {
		t.Fatalf("ReplaceProxy after a stop: %v", err)
	}
	check("a revive of the stopped proxy", true)
}

// macRefusingDocker is fakeDocker on an engine that refuses the run proxy's MAC address: at the create of any
// container whose endpoint names one, or, with atStart, at that container's start instead. refused counts the
// refusals; named holds, by id, whether each container's create named a MAC. A create under a name another
// container still holds is refused, as the engine does.
type macRefusingDocker struct {
	*fakeDocker
	atStart bool
	refused *int
	named   map[string]bool
}

func (m macRefusingDocker) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	m.mu.Lock()
	held := m.containers[opts.Name]
	m.mu.Unlock()
	if held != nil && !held.removed {
		return client.ContainerCreateResult{}, errors.New("Conflict. The container name \"/" + opts.Name + "\" is already in use")
	}
	mac := false
	if opts.NetworkingConfig != nil {
		for _, ep := range opts.NetworkingConfig.EndpointsConfig {
			mac = mac || (ep != nil && len(ep.MacAddress) > 0)
		}
	}
	if mac && !m.atStart {
		m.mu.Lock()
		*m.refused++
		m.mu.Unlock()
		return client.ContainerCreateResult{}, errors.New("invalid endpoint settings: a MAC address is not accepted here")
	}
	resp, err := m.fakeDocker.ContainerCreate(ctx, opts)
	if err == nil {
		m.mu.Lock()
		m.named[resp.ID] = mac
		m.mu.Unlock()
	}
	return resp, err
}

func (m macRefusingDocker) ContainerStart(ctx context.Context, id string, opts client.ContainerStartOptions) (client.ContainerStartResult, error) {
	m.mu.Lock()
	mac := m.named[id]
	if mac {
		*m.refused++
	}
	m.mu.Unlock()
	if mac {
		return client.ContainerStartResult{}, errors.New("failed to set up container networking: a MAC address is not accepted here")
	}
	return m.fakeDocker.ContainerStart(ctx, id, opts)
}

// TestProxyMAC_AnEngineThatRefusesItStillGetsAProxy: an engine that refuses the run's MAC address, at the create or
// at the start, still ends with one running proxy. The attempt is made once more without the address, at the
// sandbox create and at a replace, so the run keeps its egress; only the wait after a replace comes back. Each
// fallback is a warning that names the run and the step. On a runner, the first attempt's producer is settled in the
// decision sink before the second attempt takes its key.
func TestProxyMAC_AnEngineThatRefusesItStillGetsAProxy(t *testing.T) {
	for _, c := range []struct {
		name, step string
		atStart    bool
		runner     bool
	}{
		{"at the create", "creating", false, false},
		{"at the start", "starting", true, false},
		{"at the create on a runner", "creating", false, true},
		{"at the start on a runner", "starting", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			f := newFakeDocker()
			f.images["busybox:latest"] = true
			refused := 0
			m := macRefusingDocker{fakeDocker: f, atStart: c.atStart, refused: &refused, named: map[string]bool{}}
			cfg := Config{ProxyImage: "wardyn-proxy:dev"}
			if c.runner {
				cfg.InternalNetwork = "wardyn-internal"
				cfg.RunnerID, cfg.DecisionSink, cfg.Evidence = "runner-1", newFakeSink(), &memEvidence{m: map[uuid.UUID][]byte{}}
			}
			d := newWithClient(m, cfg)
			ctx := context.Background()
			spec := testSpec()
			sb, err := d.CreateSandbox(ctx, spec)
			if err != nil {
				t.Fatalf("CreateSandbox on an engine that refuses the MAC %s: %v", c.name, err)
			}
			if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
				t.Fatalf("ReplaceProxy on an engine that refuses the MAC %s: %v", c.name, err)
			}
			proxies := 0
			for _, ct := range f.containers {
				if !ct.removed && ct.cfg != nil && ct.cfg.Labels[labelRun] == spec.RunID.String() && ct.cfg.Labels[labelComponent] == componentProxy {
					proxies++
				}
			}
			p := f.containers[proxyContainerName(spec.RunID)]
			if proxies != 1 || p == nil || p.removed || p.state == nil || !p.state.Running {
				t.Fatalf("%d proxies for the run, the proxy = %+v; want exactly one, running", proxies, p)
			}
			ep := proxyEndpoint(f, spec.RunID)
			if ep == nil || len(ep.MacAddress) != 0 || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address.String() != "10.88.0.2" {
				t.Fatalf("the replaced proxy's run-network endpoint = %+v, want the pinned address 10.88.0.2 and no MAC", ep)
			}
			if refused != 2 {
				t.Fatalf("refusals of the MAC = %d, want 2 (the sandbox create's proxy and the replace's)", refused)
			}
			out := logs.String()
			if n := strings.Count(out, "with its MAC address on the run network failed"); n != 2 || !strings.Contains(out, spec.RunID.String()) {
				t.Fatalf("log = %q, want two MAC fallback warnings naming run %s", out, spec.RunID)
			}
			if n := strings.Count(out, "docker: "+c.step+" the run's proxy"); n != 2 {
				t.Fatalf("log = %q, want both warnings to begin %q", out, "docker: "+c.step)
			}
		})
	}
}

// TestProxyMAC_AFallbackTheNameBlocksAnswersTheFirstFailure: the start that named the MAC fails and the proxy it
// made cannot be removed, so the second create meets the run's own name. The replace answers the start's failure
// as ErrProxyReplaceFailed. It never answers a newer generation's refusal, which tells the caller the run's proxy
// was not touched: the old proxy is gone and none is running.
func TestProxyMAC_AFallbackTheNameBlocksAnswersTheFirstFailure(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	refused := 0
	m := macRefusingDocker{fakeDocker: f, atStart: true, refused: &refused, named: map[string]bool{}}
	d := newWithClient(m, Config{ProxyImage: "wardyn-proxy:dev"})
	ctx := context.Background()
	spec := testSpec()
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	name := proxyContainerName(spec.RunID)
	armed := false
	f.mu.Lock()
	f.onCreate = func(created string) {
		if created != name || armed {
			return
		}
		armed = true
		f.mu.Lock()
		f.failRemove = map[string]int{name: 1}
		f.mu.Unlock()
	}
	f.mu.Unlock()
	_, err = d.ReplaceProxyHashedAt(ctx, sb.Ref, renderedTestConfig(t, "fresh"), 1)
	if !errors.Is(err, runner.ErrProxyReplaceFailed) || errors.Is(err, runner.ErrProxyGenerationNewer) || !strings.Contains(err.Error(), "docker: start proxy") {
		t.Fatalf("ReplaceProxyHashedAt = %v, want ErrProxyReplaceFailed with the start's failure and no newer generation's refusal", err)
	}
}
