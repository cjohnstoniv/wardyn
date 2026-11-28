// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

var runServiceOff = Config{RunServiceOff: true}

// runAddressActions is every action, of any verb, on a Service or an EndpointSlice, as "<verb> <resource>".
func runAddressActions(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		if r := a.GetResource().Resource; r == "services" || r == "endpointslices" {
			out = append(out, a.GetVerb()+" "+r)
		}
	}
	return out
}

func TestRunServiceOff_StartPinsTheProxyPodAfterReady(t *testing.T) {
	pending := proxyRead{ip: "10.244.0.7"}
	d, cs, spec, log := serverRun(t, runServiceOff, pending, pending, readyAt7)
	installPodUIDReactor(cs)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got := runAddressActions(cs); len(got) != 0 {
		t.Errorf("start with the switch off issued %v, want no Service or EndpointSlice call", got)
	}
	if ready, created := log.index("proxy-ready-read"), log.index("agent-create"); ready < 0 || created < ready {
		t.Errorf("events = %v: the agent pod must be created after the proxy reads Ready", log.events)
	}
	pod := agentPod(t, cs, spec.RunID)
	if ha := pod.Spec.HostAliases; len(ha) != 1 || ha[0].IP != "10.244.0.7" {
		t.Errorf("hostAliases = %+v, want the proxy pod's IP 10.244.0.7", ha)
	}
	if dns := pod.Spec.DNSConfig; dns == nil || !slices.Equal(dns.Nameservers, []string{"10.244.0.7"}) {
		t.Errorf("DNSConfig = %+v, want the proxy pod's IP as the one nameserver", dns)
	}
	if o := pod.OwnerReferences; len(o) != 1 || o[0].Kind != "Pod" || o[0].Name != proxyPodName(spec.RunID) || o[0].UID == "" {
		t.Errorf("agent owners = %+v, want the proxy pod, by its UID", o)
	}
	if sb.ProxyAddr != "10.244.0.7" {
		t.Errorf("sandbox proxy address = %q, want the proxy pod's", sb.ProxyAddr)
	}

	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	if got := runAddressActions(cs); len(got) != 0 {
		t.Errorf("destroy with the switch off issued %v, want no Service or EndpointSlice call", got)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

func TestRunServiceOff_FailedStartCleanupAsksForNoService(t *testing.T) {
	d, cs, spec, _ := serverRun(t, runServiceOff, proxyRead{ip: "10.244.0.7"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := d.CreateSandbox(ctx, spec); err == nil {
		t.Fatal("CreateSandbox = nil, want the proxy's deadline")
	}
	if n := agentCreates(cs, spec.RunID); n != 0 {
		t.Errorf("agent pod created %d time(s) for a proxy that never became Ready, want 0", n)
	}
	if got := runAddressActions(cs); len(got) != 0 {
		t.Errorf("a failed start's cleanup issued %v, want no Service or EndpointSlice call", got)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

func TestRunServiceOff_SweepAsksForNoService(t *testing.T) {
	d, cs, spec, _ := serverRun(t, runServiceOff, readyAt7)
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	agePodsOfRun(t, cs, spec.RunID, time.Hour)
	if swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan); err != nil || swept != 1 {
		t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 1, nil", swept, err)
	}
	if got := runAddressActions(cs); len(got) != 0 {
		t.Errorf("start and sweep with the switch off issued %v, want no Service or EndpointSlice call", got)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

// Boot asks for no Service or EndpointSlice verb with the switch off, even with the agent-sandbox backend on.
func TestRunServiceOff_BootNeedsNoServiceVerbs(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	cs.Resources = servedSandboxes()
	var asked []string
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		in := a.(clienttesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview).DeepCopy()
		ra := in.Spec.ResourceAttributes
		in.Status.Allowed = ra.Resource != "services" && ra.Resource != "endpointslices"
		if !in.Status.Allowed {
			asked = append(asked, ra.Verb+" "+ra.Resource)
		}
		return true, in, nil
	})
	useDynamicClient(t, newFakeDynamic())
	cfg := Config{AgentSandbox: true, RunServiceOff: true, Namespaces: []string{testNamespace}, ProxyImage: "p"}
	if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err != nil {
		t.Fatalf("newWithClient = %v, want boot with every Service and EndpointSlice verb denied", err)
	}
	if len(asked) != 0 {
		t.Errorf("boot asked for %v, want no Service or EndpointSlice verb", asked)
	}
}

func TestRunServiceOff_KeepRefusesToBoot(t *testing.T) {
	err := checkAgentSandboxKeep(Config{AgentSandbox: true, AgentSandboxKeep: true, RunServiceOff: true})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_K8S_AGENT_SANDBOX_KEEP needs WARDYN_K8S_RUN_SERVICE on") {
		t.Fatalf("keep with the switch off: err = %v, want the refusal naming both variables", err)
	}
	if err := checkAgentSandboxKeep(Config{AgentSandbox: true, RunServiceOff: true}); err != nil {
		t.Errorf("the switch off without keep: err = %v, want nil", err)
	}
	t.Setenv("WARDYN_K8S_RUN_SERVICE", "")
	if buildConfig(substrate.Deps{}).RunServiceOff {
		t.Error("buildConfig with WARDYN_K8S_RUN_SERVICE unset turned the run Service off, want it on")
	}
	t.Setenv("WARDYN_K8S_RUN_SERVICE", "off")
	if !buildConfig(substrate.Deps{}).RunServiceOff {
		t.Error("buildConfig did not read WARDYN_K8S_RUN_SERVICE=off")
	}
}

// With the switch off the agent starts after its proxy is Ready, so no proxy wait is added; a run's own value is kept
// either way, and with the switch on a run without one gets the whole start budget.
func TestRunServiceOff_ProxyWaitEnv(t *testing.T) {
	for _, tc := range []struct {
		name     string
		off      bool
		override bool
	}{
		{"off without an override", true, false},
		{"off with an override", true, true},
		{"on without an override", false, false},
		{"on with an override", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, spec, _ := serverRun(t, Config{RunServiceOff: tc.off}, readyAt7)
			spec.Interactive = true
			if tc.override {
				spec.Env[proxyWaitSecsEnv] = "42"
			}
			if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			got, set := envValue(agentPod(t, cs, spec.RunID), proxyWaitSecsEnv)
			budget := strconv.Itoa(int((d.cfg.StartTimeout+d.cfg.CapacityWait)/time.Second) + 1)
			switch {
			case tc.override && got != "42":
				t.Errorf("%s = %q (set %v), want the run's own 42", proxyWaitSecsEnv, got, set)
			case !tc.override && tc.off && set:
				t.Errorf("%s = %q, want it absent with the switch off", proxyWaitSecsEnv, got)
			case !tc.override && !tc.off && got != budget:
				t.Errorf("%s = %q (set %v), want the start budget %s", proxyWaitSecsEnv, got, set, budget)
			}
		})
	}
}

// The constructor every path shares refuses keep with the switch off before it calls the API, so NewSubstrate, which
// skips the registry's checks, refuses with the same message.
func TestRunServiceOff_ConstructorRefusesKeepBeforeAnyCall(t *testing.T) {
	cs := fake.NewClientset()
	cfg := Config{AgentSandbox: true, AgentSandboxKeep: true, RunServiceOff: true, Namespaces: []string{testNamespace}, ProxyImage: "p"}
	if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); !errors.Is(err, errKeepNeedsRunService) {
		t.Fatalf("newWithClient = %v, want errKeepNeedsRunService", err)
	}
	if got := cs.Actions(); len(got) != 0 {
		t.Errorf("the refusal came after %d API call(s): %v", len(got), got)
	}
}
