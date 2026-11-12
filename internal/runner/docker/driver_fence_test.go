// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fencedRun is a created sandbox on a fake daemon: the driver, the fake, the agent's ref and the proxy's name.
type fencedRun struct {
	d     *Driver
	f     *fakeDocker
	ref   string
	proxy string
}

func newFencedRun(t *testing.T) fencedRun {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return fencedRun{d: d, f: f, ref: sb.Ref, proxy: proxyContainerName(testSpec().RunID)}
}

// labelProxy stamps the running proxy as a replace for generation gen would have; gen 0 leaves it unlabelled.
func (r fencedRun) labelProxy(gen int) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if gen > 0 {
		r.f.containers[r.proxy].cfg.Labels[labelProxyGeneration] = fmt.Sprint(gen)
	}
}

func (r fencedRun) endpoints(t *testing.T, name string) int {
	t.Helper()
	res, err := r.d.cli.ContainerInspect(context.Background(), name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return len(res.Container.NetworkSettings.Networks)
}

func TestFenceProxy_AbsentAgentIsNil(t *testing.T) {
	d := newTestDriver(newFakeDocker())
	if err := d.FenceProxy(context.Background(), agentContainerName(uuid.New())); err != nil {
		t.Fatalf("FenceProxy of an agent the engine does not hold = %v; want nil", err)
	}
}

func TestFenceProxy_VMIsUnsupported(t *testing.T) {
	d := newTestDriver(newFakeDocker())
	ref := agentContainerName(uuid.New())
	d.vms.Store(ref, &vmRun{})
	if err := d.FenceProxy(context.Background(), ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Fatalf("FenceProxy of a VM = %v; want runner.ErrEndUnsupported", err)
	}
}

// A second network the agent joined is cut as well as its run network, and the proxy is stopped after.
func TestFenceProxy_CutsEveryNetwork(t *testing.T) {
	r := newFencedRun(t)
	ctx := context.Background()
	if _, err := r.d.cli.NetworkConnect(ctx, "wardyn-extra", client.NetworkConnectOptions{Container: r.ref}); err != nil {
		t.Fatal(err)
	}
	if n := r.endpoints(t, r.ref); n != 2 {
		t.Fatalf("agent endpoints before the fence = %d; want 2", n)
	}
	if err := r.d.FenceProxy(ctx, r.ref); err != nil {
		t.Fatalf("FenceProxy: %v", err)
	}
	if n := r.endpoints(t, r.ref); n != 0 {
		t.Errorf("agent endpoints after the fence = %d; want 0", n)
	}
	if p := r.f.containers[r.proxy]; p == nil || !p.removed {
		t.Error("the fence left the proxy running")
	}
}

// A network the agent cannot be taken off is an error naming it, never a nil that left an endpoint.
func TestFenceProxy_ANetworkItCannotLeaveIsAnError(t *testing.T) {
	r := newFencedRun(t)
	r.f.failDisconnect = map[string]bool{internalNetName(testSpec().RunID): true}
	err := r.d.FenceProxy(context.Background(), r.ref)
	if err == nil {
		t.Fatal("FenceProxy with an endpoint left = nil; want an error")
	}
	if want := internalNetName(testSpec().RunID); !strings.Contains(err.Error(), want) {
		t.Errorf("FenceProxy error %q does not name the network %s", err, want)
	}
}

func TestFenceProxy_ConcurrentFencers(t *testing.T) {
	r := newFencedRun(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = r.d.FenceProxy(ctx, r.ref)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("fencer %d = %v; want nil", i, err)
		}
	}
	if n := r.endpoints(t, r.ref); n != 0 {
		t.Errorf("agent endpoints after eight fencers = %d; want 0", n)
	}
}

func TestReplaceProxyHashedAt_RefusesNewerLabel(t *testing.T) {
	r := newFencedRun(t)
	r.labelProxy(5)
	before := r.f.containers[r.proxy]
	_, err := r.d.ReplaceProxyHashedAt(context.Background(), r.ref, renderedTestConfig(t, "fresh"), 4)
	if !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("replace at 4 over a proxy labelled 5 = %v; want runner.ErrProxyGenerationNewer", err)
	}
	if after := r.f.containers[r.proxy]; after != before || after.removed {
		t.Error("a refused replace touched the proxy")
	}
}

func TestReplaceProxyHashedAt_RefusesEqualLabel(t *testing.T) {
	r := newFencedRun(t)
	r.labelProxy(4)
	before := r.f.containers[r.proxy]
	_, err := r.d.ReplaceProxyHashedAt(context.Background(), r.ref, renderedTestConfig(t, "fresh"), 4)
	if !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("replace at 4 over a proxy labelled 4 = %v; want runner.ErrProxyGenerationNewer", err)
	}
	if after := r.f.containers[r.proxy]; after != before || after.removed {
		t.Error("a refused replace touched the proxy")
	}
}

// The compare for every request against an unlabelled proxy and one labelled 4: a request of 0 is never refused,
// and any request is accepted over an unlabelled proxy.
func TestReplaceProxyHashedAt_LabelValues(t *testing.T) {
	for _, tc := range []struct {
		label, req int
		refused    bool
	}{
		{0, 0, false}, {0, 3, false}, {0, 4, false}, {0, 5, false},
		{4, 0, false}, {4, 3, true}, {4, 4, true}, {4, 5, false},
	} {
		t.Run(fmt.Sprintf("label%d_request%d", tc.label, tc.req), func(t *testing.T) {
			r := newFencedRun(t)
			r.labelProxy(tc.label)
			_, err := r.d.ReplaceProxyHashedAt(context.Background(), r.ref, renderedTestConfig(t, "fresh"), tc.req)
			if got := errors.Is(err, runner.ErrProxyGenerationNewer); got != tc.refused {
				t.Fatalf("replace at %d over label %d: err = %v; want refused = %v", tc.req, tc.label, err, tc.refused)
			}
			if !tc.refused && err != nil {
				t.Fatalf("replace at %d over label %d = %v; want it to succeed", tc.req, tc.label, err)
			}
		})
	}
}

// swapOnRemoveDocker models a replace that lost a race: the first removal of the proxy it inspected finds
// another proxy under the same name, started for a newer generation, in the instant between.
type swapOnRemoveDocker struct {
	*fakeDocker
	armed  bool
	oldID  string
	name   string
	newer  *createdContainer
	newest string
}

func (s *swapOnRemoveDocker) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if s.armed && (id == s.oldID || id == s.name) {
		s.armed = false
		s.mu.Lock()
		s.containers[s.name] = s.newer
		s.containers[s.newest] = s.newer
		if s.createConflicts == nil {
			s.createConflicts = map[string]bool{}
		}
		s.createConflicts[s.name] = true
		s.mu.Unlock()
	}
	return s.fakeDocker.ContainerRemove(ctx, id, o)
}

func TestReplaceProxyHashedAt_RemovesOnlyTheInspectedID(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	runID := testSpec().RunID
	name := proxyContainerName(runID)
	w := &swapOnRemoveDocker{fakeDocker: f, oldID: "old-proxy-id", name: name, newest: "newer-proxy-id"}
	d := newWithClient(w, Config{ProxyImage: "wardyn-proxy:dev"})
	ctx := context.Background()
	sb, err := d.CreateSandbox(ctx, testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	old := f.containers[name]
	f.mu.Lock()
	old.id = w.oldID
	f.containers[w.oldID] = old // the id is the name's other face, as a daemon has two
	f.mu.Unlock()
	w.newer = &createdContainer{
		name:  name,
		id:    w.newest,
		cfg:   &container.Config{Labels: map[string]string{labelProxyGeneration: "9"}},
		host:  &container.HostConfig{NetworkMode: container.NetworkMode(internalNetName(runID))},
		state: &container.State{Status: "running", Running: true},
	}
	w.armed = true

	_, err = d.ReplaceProxyHashedAt(ctx, sb.Ref, renderedTestConfig(t, "fresh"), 7)
	if !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("replace that finds a proxy for generation 9 under the name = %v; want runner.ErrProxyGenerationNewer", err)
	}
	if w.newer.removed {
		t.Error("the replace removed the proxy that took the name after its inspect")
	}
	if !old.removed {
		t.Error("the replace did not remove the proxy it inspected")
	}
}

func TestReplaceProxyHashedAt_StampsTheLabel(t *testing.T) {
	r := newFencedRun(t)
	if _, err := r.d.ReplaceProxyHashedAt(context.Background(), r.ref, renderedTestConfig(t, "fresh"), 3); err != nil {
		t.Fatalf("ReplaceProxyHashedAt: %v", err)
	}
	p := r.f.containers[r.proxy]
	if p == nil || p.removed {
		t.Fatal("no replacement proxy")
	}
	if got := p.cfg.Labels[labelProxyGeneration]; got != "3" {
		t.Errorf("replacement proxy label = %q; want %q", got, "3")
	}
}

func TestReplaceProxyHashedAt_RefusesACutAgent(t *testing.T) {
	r := newFencedRun(t)
	ctx := context.Background()
	runID := testSpec().RunID
	if _, err := r.d.cli.NetworkDisconnect(ctx, internalNetName(runID), client.NetworkDisconnectOptions{Container: r.ref, Force: true}); err != nil {
		t.Fatal(err)
	}
	before := r.f.containers[r.proxy]
	for _, gen := range []int{0, 5} {
		if _, err := r.d.ReplaceProxyHashedAt(ctx, r.ref, renderedTestConfig(t, "fresh"), gen); !errors.Is(err, runner.ErrProxyFenced) {
			t.Fatalf("replace at %d for a cut agent = %v; want runner.ErrProxyFenced", gen, err)
		}
	}
	if after := r.f.containers[r.proxy]; after != before || after.removed {
		t.Error("a refused replace removed the proxy")
	}
}

func TestReplaceProxyHashed_ZeroIsUnchanged(t *testing.T) {
	r := newFencedRun(t)
	r.labelProxy(9)
	old := r.f.containers[r.proxy]
	if _, err := r.d.ReplaceProxyHashed(context.Background(), r.ref, renderedTestConfig(t, "fresh")); err != nil {
		t.Fatalf("ReplaceProxyHashed over a labelled proxy = %v; want it to replace, as before a generation existed", err)
	}
	if !old.removed || !old.forceRemoved {
		t.Error("the old proxy was not force-removed")
	}
	p := r.f.containers[r.proxy]
	if p == old || p == nil || p.removed {
		t.Fatal("no replacement proxy")
	}
	if got, ok := p.cfg.Labels[labelProxyGeneration]; ok {
		t.Errorf("a replace without a generation labelled the proxy %q; want no label", got)
	}
}

func TestStopProxyAt_LeavesANewerProxy(t *testing.T) {
	r := newFencedRun(t)
	r.labelProxy(5)
	ctx := context.Background()
	proxy, agent := r.f.containers[r.proxy], r.f.containers[r.ref]
	for _, gen := range []int{3, 4} {
		if err := r.d.StopProxyAt(ctx, r.ref, gen); !errors.Is(err, runner.ErrProxyGenerationNewer) {
			t.Fatalf("StopProxyAt %d over a proxy labelled 5 = %v; want runner.ErrProxyGenerationNewer", gen, err)
		}
	}
	if proxy.removed || proxy.state.Status != "running" || agent.state.Status != "running" {
		t.Fatalf("a refused stop touched the run: proxy removed=%v state=%s, agent state=%s", proxy.removed, proxy.state.Status, agent.state.Status)
	}
	if err := r.d.StopProxyAt(ctx, r.ref, 5); err != nil {
		t.Fatalf("StopProxyAt 5 over a proxy labelled 5: %v", err)
	}
	if !proxy.removed {
		t.Error("a stop for the proxy's own generation left it")
	}
	if agent.state.Status != "running" {
		t.Errorf("StopProxyAt stopped the agent (%s); only a proxy that will not go does that", agent.state.Status)
	}
}

func TestEndSandboxAt_LeavesANewerRun(t *testing.T) {
	r := newFencedRun(t)
	r.labelProxy(5)
	ctx := context.Background()
	proxy, agent := r.f.containers[r.proxy], r.f.containers[r.ref]
	if err := r.d.EndSandboxAt(ctx, r.ref, 4); !errors.Is(err, runner.ErrProxyGenerationNewer) {
		t.Fatalf("EndSandboxAt 4 over a proxy labelled 5 = %v; want runner.ErrProxyGenerationNewer", err)
	}
	if proxy.removed || agent.state.Status != "running" {
		t.Fatalf("a refused end touched the run: proxy removed=%v, agent state=%s", proxy.removed, agent.state.Status)
	}
	if err := r.d.EndSandboxAt(ctx, r.ref, 5); err != nil {
		t.Fatalf("EndSandboxAt 5: %v", err)
	}
	if !proxy.removed || agent.removed || agent.state.Status != "exited" {
		t.Errorf("end at the proxy's generation: proxy removed=%v, agent removed=%v state=%s; want the proxy gone and the agent stopped and kept",
			proxy.removed, agent.removed, agent.state.Status)
	}
}

// liveFenceAgent starts a bare agent-named container on a network of its own, as a contained run's agent is
// on its run network, and answers its name. Nothing else of the run exists.
func liveFenceAgent(ctx context.Context, t *testing.T, d *Driver) (name string) {
	t.Helper()
	runID := uuid.New()
	netName := internalNetName(runID)
	if _, err := d.cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{Driver: "bridge", Internal: true}); err != nil {
		t.Fatalf("create network: %v", err)
	}
	name = agentContainerName(runID)
	t.Cleanup(func() {
		_, _ = d.cli.ContainerRemove(context.Background(), name, client.ContainerRemoveOptions{Force: true})
		_, _ = d.cli.NetworkRemove(context.Background(), netName, client.NetworkRemoveOptions{})
	})
	if _, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: "busybox:latest", Cmd: []string{"sleep", "600"}, Labels: map[string]string{labelRun: runID.String()}},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		Name:       name,
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := d.cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	return name
}

func liveEndpoints(ctx context.Context, t *testing.T, d *Driver, name string) int {
	t.Helper()
	res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return len(res.Container.NetworkSettings.Networks)
}

// TestFenceProxy_LeavesZeroEndpoints on a real engine with the real proxy: the agent reaches its proxy before
// the fence, and after it has no endpoint and no path to the proxy's pinned address.
func TestFenceProxy_LeavesZeroEndpoints(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live fence test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	d, err := New(Config{ProxyImage: liveProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	cpURL, cpCA := liveRenewingControlPlane(ctx, t, d)
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            "busybox:latest",
		ConfinementClass: types.CC1,
		ProxyConfig: runner.ProxyConfig{
			RunToken:          "fence-test",
			ControlPlaneURL:   cpURL,
			ControlPlaneCAPEM: cpCA,
			Policy:            types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"example.com"}},
		},
		Resources: runner.Resources{CPUMillis: 500, MemoryMiB: 128},
		Labels:    map[string]string{"wardyn.test": "fence"},
	}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })

	res, err := d.cli.ContainerInspect(ctx, proxyContainerName(spec.RunID), client.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("inspect proxy: %v", err)
	}
	ep := res.Container.NetworkSettings.Networks[internalNetName(spec.RunID)]
	if ep == nil || !ep.IPAddress.IsValid() {
		t.Fatalf("proxy has no address on %s", internalNetName(spec.RunID))
	}
	pinned := fmt.Sprintf("http://%s:%d", ep.IPAddress, runner.ProxyListenPort)
	if ok, out := proxyAnswered(ctx, t, d, sb.Ref, pinned); !ok {
		t.Fatalf("before the fence the agent got no answer from its proxy at %s:\n%s", pinned, out)
	}

	if err := d.FenceProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("FenceProxy: %v", err)
	}
	if n := liveEndpoints(ctx, t, d, sb.Ref); n != 0 {
		t.Errorf("agent endpoints after the fence = %d; want 0", n)
	}
	if ok, out := proxyAnswered(ctx, t, d, sb.Ref, pinned); ok {
		t.Errorf("after the fence the agent still got an answer at %s:\n%s", pinned, out)
	}
	if err := d.FenceProxy(ctx, sb.Ref); err != nil {
		t.Errorf("FenceProxy again = %v; want nil", err)
	}
}

// TestFenceProxy_StoppedContainerLeavesZeroEndpoints: an exited agent is cut too, so a probe that finds its
// container stopped is not the same as a run that was fenced.
func TestFenceProxy_StoppedContainerLeavesZeroEndpoints(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live fence test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := New(Config{ProxyImage: liveProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	name := liveFenceAgent(ctx, t, d)
	timeout := 1
	if _, err := d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("stop agent: %v", err)
	}
	if n := liveEndpoints(ctx, t, d, name); n == 0 {
		t.Fatal("an exited agent already shows no endpoint; the test would prove nothing")
	}
	if err := d.FenceProxy(ctx, name); err != nil {
		t.Fatalf("FenceProxy of an exited agent: %v", err)
	}
	if n := liveEndpoints(ctx, t, d, name); n != 0 {
		t.Errorf("an exited agent's endpoints after the fence = %d; want 0", n)
	}
}

// TestFenceProxy_StartAfterFenceStaysCut: a fenced, exited agent started again has no endpoint. This is what
// lets a recorded fence stand: if Docker reconnected the container on start, it would not hold.
func TestFenceProxy_StartAfterFenceStaysCut(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live fence test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := New(Config{ProxyImage: liveProxyImage})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	name := liveFenceAgent(ctx, t, d)
	timeout := 1
	if _, err := d.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("stop agent: %v", err)
	}
	if err := d.FenceProxy(ctx, name); err != nil {
		t.Fatalf("FenceProxy: %v", err)
	}
	if _, err := d.cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start the fenced agent: %v", err)
	}
	if n := liveEndpoints(ctx, t, d, name); n != 0 {
		res, _ := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		t.Fatalf("a fenced agent started again has %d endpoints; want 0 (inspect networks: %+v)", n, res.Container.NetworkSettings.Networks)
	}
}
