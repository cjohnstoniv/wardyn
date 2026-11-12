// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noNetworkSpec is testSandboxSpec for a No network run whose proxy reaches Wardyn at controlPlaneURL.
func noNetworkSpec(controlPlaneURL string) runner.SandboxSpec {
	spec := testSandboxSpec()
	spec.ProxyConfig.ControlPlaneURL = controlPlaneURL
	spec.ProxyConfig.NoNetwork = true
	return spec
}

// noNetworkPeer is wardynd's pods as the chart names them.
func noNetworkPeer() *ControlPlanePeer {
	return &ControlPlanePeer{
		Namespace: "wardyn-system",
		PodLabels: map[string]string{
			"app.kubernetes.io/name":      "wardyn",
			"app.kubernetes.io/instance":  "rel",
			"app.kubernetes.io/component": "control-plane",
		},
	}
}

// noNetworkLookup answers every host with addrs, and records the hosts asked.
func noNetworkLookup(asked *[]string, addrs []string, err error) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, host string) ([]string, error) {
		*asked = append(*asked, host)
		return addrs, err
	}
}

func findNetPol(t *testing.T, cs *fake.Clientset, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	list, err := cs.NetworkingV1().NetworkPolicies("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list NetworkPolicies: %v", err)
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}
	t.Fatalf("NetworkPolicy %s was not created", name)
	return nil
}

func findPod(t *testing.T, cs *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}
	t.Fatalf("pod %s was not created", name)
	return nil
}

// createSandboxOn creates spec on d, whose proxy pod's IP and agent pod's state are scripted.
func createSandboxOn(t *testing.T, d *Driver, cs *fake.Clientset, spec runner.SandboxSpec) runner.Sandbox {
	t.Helper()
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return sb
}

func hasPort53(rules []networkingv1.NetworkPolicyEgressRule) bool {
	for _, r := range rules {
		for _, p := range r.Ports {
			if p.Port != nil && p.Port.IntVal == 53 {
				return true
			}
		}
	}
	return false
}

func hasIPBlock(rules []networkingv1.NetworkPolicyEgressRule) bool {
	for _, r := range rules {
		for _, p := range r.To {
			if p.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

// A No network run on a server: its proxy's policy has one rule, TCP on the URL's port to wardynd's pods, and
// its pod pins Wardyn's name to the one address. Catches a DNS rule or address range left in, a missing pin
// and the wrong default port.
func TestCreateSandbox_NoNetworkServerProxyReachesOnlyWardyn(t *testing.T) {
	var asked []string
	d, cs := newTestDriver(t, Config{
		ControlPlanePeer: noNetworkPeer(),
		LookupHost:       noNetworkLookup(&asked, []string{"10.96.0.9"}, nil),
	})
	spec := noNetworkSpec("https://wardynd.wardyn-system.svc.cluster.local:8443")
	createSandboxOn(t, d, cs, spec)

	np := findNetPol(t, cs, proxyNetPolName(spec.RunID))
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("proxy egress = %d rules (%+v), want exactly 1", len(np.Spec.Egress), np.Spec.Egress)
	}
	if hasPort53(np.Spec.Egress) || hasIPBlock(np.Spec.Egress) {
		t.Errorf("proxy egress has a DNS rule or an address range: %+v", np.Spec.Egress)
	}
	rule := np.Spec.Egress[0]
	if len(rule.Ports) != 1 || rule.Ports[0].Protocol == nil || *rule.Ports[0].Protocol != corev1.ProtocolTCP ||
		rule.Ports[0].Port == nil || rule.Ports[0].Port.IntVal != 8443 {
		t.Errorf("proxy egress ports = %+v, want TCP/8443 only", rule.Ports)
	}
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector == nil || rule.To[0].PodSelector == nil {
		t.Fatalf("proxy egress peers = %+v, want one namespace-and-pod peer", rule.To)
	}
	if diff := cmp.Diff(map[string]string{"kubernetes.io/metadata.name": "wardyn-system"}, rule.To[0].NamespaceSelector.MatchLabels); diff != "" {
		t.Errorf("peer namespace selector: %s", diff)
	}
	if diff := cmp.Diff(noNetworkPeer().PodLabels, rule.To[0].PodSelector.MatchLabels); diff != "" {
		t.Errorf("peer pod selector: %s", diff)
	}

	wantAlias := corev1.HostAlias{IP: "10.96.0.9", Hostnames: []string{"wardynd.wardyn-system.svc.cluster.local"}}
	pod := findPod(t, cs, proxyPodName(spec.RunID))
	if diff := cmp.Diff([]corev1.HostAlias{wantAlias}, pod.Spec.HostAliases); diff != "" {
		t.Errorf("proxy pod hostAliases: %s", diff)
	}
	if len(asked) != 1 || asked[0] != "wardynd.wardyn-system.svc.cluster.local" {
		t.Errorf("lookups = %v, want one, of Wardyn's internal name", asked)
	}

	// The agent policy is the same as an ordinary run's: the same run id on a fresh driver, without the switch.
	d2, cs2 := newTestDriver(t, Config{})
	ordinary := testSandboxSpec()
	ordinary.RunID = spec.RunID
	createSandboxOn(t, d2, cs2, ordinary)
	want := findNetPol(t, cs2, agentNetPolName(spec.RunID)).Spec
	if diff := cmp.Diff(want, findNetPol(t, cs, agentNetPolName(spec.RunID)).Spec); diff != "" {
		t.Errorf("a No network run's agent policy differs from an ordinary run's: %s", diff)
	}
}

// With no port the proxy's own default for the scheme is the rule's port, and an address literal needs no pin.
func TestCreateSandbox_NoNetworkPortDefaultsByScheme(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantPort int32
		wantPin  bool
	}{
		{"https://wardynd.wardyn-system.svc.cluster.local", 443, true},
		{"http://127.0.0.1", 80, false}, // loopback: the one plaintext host the proxy accepts
		{"https://10.96.0.9:9443", 9443, false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			var asked []string
			d, cs := newTestDriver(t, Config{
				ControlPlanePeer: noNetworkPeer(),
				LookupHost:       noNetworkLookup(&asked, []string{"10.96.0.9"}, nil),
			})
			spec := noNetworkSpec(tc.url)
			createSandboxOn(t, d, cs, spec)
			np := findNetPol(t, cs, proxyNetPolName(spec.RunID))
			if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].Ports) != 1 || np.Spec.Egress[0].Ports[0].Port.IntVal != tc.wantPort {
				t.Errorf("egress = %+v, want one rule on port %d", np.Spec.Egress, tc.wantPort)
			}
			pod := findPod(t, cs, proxyPodName(spec.RunID))
			if pinned := len(pod.Spec.HostAliases) > 0; pinned != tc.wantPin {
				t.Errorf("hostAliases = %+v, want pinned %v", pod.Spec.HostAliases, tc.wantPin)
			}
			if !tc.wantPin && len(asked) != 0 {
				t.Errorf("an address literal was looked up: %v", asked)
			}
		})
	}
}

// newNoNetworkRunnerHarness is a runner-mode driver, with runnerd's peer set, that can create a sandbox.
func newNoNetworkRunnerHarness(t *testing.T, peer *RunnerdPeer) *runnerHarness {
	t.Helper()
	px := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(px.Close)
	h := newRunnerHarness(t)
	h.d.cfg.ProxyImage = "registry.example/wardyn-proxy:pinned"
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	h.d.cfg.RunnerdPeer = peer
	installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
	installAgentRunningReactor(t, h.cs)
	return h
}

func testRunnerdPeer() *RunnerdPeer {
	return &RunnerdPeer{Namespace: "wardyn-runner", PodLabels: map[string]string{"app": "runnerd"}, RelayPort: 8443, SinkPort: 8444}
}

// A No network run on a runner: its proxy's egress is runnerd's relay and sink ports and nothing else, and the
// relay's hostAlias stays.
func TestCreateSandbox_NoNetworkRunnerProxyIsRunnerdOnly(t *testing.T) {
	h := newNoNetworkRunnerHarness(t, testRunnerdPeer())
	spec := noNetworkSpec("https://wardynd.wardyn-system.svc:8443")
	spec.RunID = h.runID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, spec); err != nil {
		t.Fatal(err)
	}
	np := findNetPol(t, h.cs, proxyNetPolName(spec.RunID))
	if diff := cmp.Diff(h.d.runnerdPeerRules(), np.Spec.Egress); diff != "" {
		t.Errorf("proxy egress is not runnerd's peer rules alone: %s", diff)
	}
	if hasPort53(np.Spec.Egress) || hasIPBlock(np.Spec.Egress) {
		t.Errorf("proxy egress has a DNS rule or an address range: %+v", np.Spec.Egress)
	}
	pod := findPod(t, h.cs, proxyPodName(spec.RunID))
	want := []corev1.HostAlias{{IP: "10.96.0.40", Hostnames: []string{"wardynd.wardyn-system.svc"}}}
	if diff := cmp.Diff(want, pod.Spec.HostAliases); diff != "" {
		t.Errorf("proxy pod hostAliases (the relay's): %s", diff)
	}
}

// goldenProxyEgress is today's three-part proxy egress, written out: DNS to the cluster's DNS pods and every
// address but the metadata one, then every address but the metadata one.
func goldenProxyEgress() []networkingv1.NetworkPolicyEgressRule {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	p53 := intstr.FromInt32(53)
	return []networkingv1.NetworkPolicyEgressRule{
		{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p53}, {Protocol: &udp, Port: &p53}},
			To: []networkingv1.NetworkPolicyPeer{
				{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.169.254/32"}}},
				{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
				},
			},
		},
		{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.169.254/32"}}}}},
	}
}

// Any other run keeps today's three rules, in server and runner mode. Catches the move changing other runs.
func TestCreateSandbox_OtherRunsKeepTheirProxyPolicy(t *testing.T) {
	t.Run("server", func(t *testing.T) {
		d, cs := newTestDriver(t, Config{ControlPlanePeer: noNetworkPeer()}) // a peer alone changes nothing
		spec := testSandboxSpec()
		sb := createSandboxOn(t, d, cs, spec)
		if diff := cmp.Diff(goldenProxyEgress(), findNetPol(t, cs, proxyNetPolName(spec.RunID)).Spec.Egress); diff != "" {
			t.Errorf("server proxy egress: %s", diff)
		}
		if sb.NoNetworkProxyOnly != "" {
			t.Errorf("NoNetworkProxyOnly = %q on an ordinary run, want empty", sb.NoNetworkProxyOnly)
		}
	})
	t.Run("runner", func(t *testing.T) {
		h := newNoNetworkRunnerHarness(t, testRunnerdPeer())
		spec := testSandboxSpec()
		spec.RunID = h.runID
		spec.ProxyConfig.ControlPlaneURL = "https://wardynd.wardyn-system.svc:8443"
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := h.d.CreateSandbox(ctx, spec); err != nil {
			t.Fatal(err)
		}
		want := append(goldenProxyEgress(), h.d.runnerdPeerRules()...)
		if diff := cmp.Diff(want, findNetPol(t, h.cs, proxyNetPolName(spec.RunID)).Spec.Egress); diff != "" {
			t.Errorf("runner proxy egress: %s", diff)
		}
	})
}

// assertNothingCreated fails on any create of a NetworkPolicy, Secret or pod, and on any touch of a claim,
// among the actions the fake saw since its last ClearActions.
func assertNothingCreated(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	for _, a := range cs.Actions() {
		switch r := a.GetResource().Resource; {
		case r == "persistentvolumeclaims":
			t.Errorf("a refused No network create touched a claim: %s %s", a.GetVerb(), r)
		case a.GetVerb() == "create" && (r == "networkpolicies" || r == "secrets" || r == "pods"):
			t.Errorf("a refused No network create made a %s", r)
		}
	}
}

// A No network create that cannot build its policy is refused with a plain error before anything exists, a
// fresh drive's claim included. Catches the plan placed after the claims, or after any create.
func TestCreateSandbox_NoNetworkRefusedBeforeAnyObject(t *testing.T) {
	const host = "wardynd.wardyn-system.svc.cluster.local"
	withDrive := func(spec runner.SandboxSpec) runner.SandboxSpec {
		spec.Drives = []types.DriveMount{*testDriveMount()}
		return spec
	}
	for _, tc := range []struct {
		name    string
		peer    *ControlPlanePeer
		addrs   []string
		lookErr error
		want    string
	}{
		{"no peer", nil, []string{"10.96.0.9"}, nil,
			"k8s: a No network run needs WARDYN_K8S_CONTROL_PLANE_NAMESPACE and WARDYN_K8S_CONTROL_PLANE_POD_LABELS set, so its proxy can reach only Wardyn; the run was not started"},
		{"no address", noNetworkPeer(), nil, nil,
			"k8s: a No network run needs Wardyn's internal address " + host + " to resolve to one IP, and it resolved to []; the run was not started"},
		{"two addresses", noNetworkPeer(), []string{"10.96.0.9", "fd00::9"}, nil,
			"k8s: a No network run needs Wardyn's internal address " + host + " to resolve to one IP, and it resolved to [10.96.0.9 fd00::9]; the run was not started"},
		{"lookup error", noNetworkPeer(), nil, errors.New("no such host"),
			"k8s: a No network run needs Wardyn's internal address " + host + " to resolve to one IP, and its lookup failed (no such host); the run was not started"},
	} {
		t.Run("server "+tc.name, func(t *testing.T) {
			var asked []string
			d, cs := newTestDriver(t, Config{ControlPlanePeer: tc.peer, LookupHost: noNetworkLookup(&asked, tc.addrs, tc.lookErr)})
			cs.ClearActions()
			_, err := d.CreateSandbox(context.Background(), withDrive(noNetworkSpec("https://"+host+":8443")))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "<nil>") {
				t.Errorf("the error prints a nil: %q", err)
			}
			assertNothingCreated(t, cs)
		})
	}
	t.Run("runner without runnerd's peer", func(t *testing.T) {
		h := newNoNetworkRunnerHarness(t, nil)
		h.cs.ClearActions()
		spec := withDrive(noNetworkSpec("https://" + host + ":8443"))
		spec.RunID = h.runID
		_, err := h.d.CreateSandbox(context.Background(), spec)
		want := "k8s: a No network run on a runner needs the runner's own pods as its proxy's only peer; the run was not started"
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		assertNothingCreated(t, h.cs)
	})
}

// A No network run in a namespace only an override admitted records that it has the proxy layer only, and the
// policy is still the narrow one. Catches a run under an override that records nothing, and a No network
// sandbox left without a value.
func TestCreateSandbox_NoNetworkUnderAnOverrideIsProxyOnly(t *testing.T) {
	setVerdict := func(d *Driver, state netPolState) {
		d.netPol.Store(&netPolVerdict{byNS: map[string]nsVerdict{testNamespace: {state: state}}})
	}
	for _, tc := range []struct {
		name  string
		state netPolState
		want  string
	}{
		{"proven", netPolProven, "pod_enforced"},
		{"unenforced", netPolUnenforced, "netpol_not_enforced"},
		{"acknowledged", netPolAcknowledged, "netpol_ambient_acknowledged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			d, cs := newTestDriver(t, Config{
				ControlPlanePeer:      noNetworkPeer(),
				LookupHost:            noNetworkLookup(&asked, []string{"10.96.0.9"}, nil),
				AllowUnenforcedNetPol: tc.state == netPolUnenforced,
				AckAmbientDefaultDeny: tc.state == netPolAcknowledged,
			})
			setVerdict(d, tc.state)
			spec := noNetworkSpec("https://wardynd.wardyn-system.svc.cluster.local:8443")
			sb := createSandboxOn(t, d, cs, spec)
			if sb.NoNetworkProxyOnly != tc.want {
				t.Errorf("NoNetworkProxyOnly = %q, want %q", sb.NoNetworkProxyOnly, tc.want)
			}
			if got := findNetPol(t, cs, proxyNetPolName(spec.RunID)).Spec.Egress; len(got) != 1 || hasPort53(got) || hasIPBlock(got) {
				t.Errorf("the proxy policy under %s is not the narrow one: %+v", tc.name, got)
			}
		})
	}

	d, _ := newTestDriver(t, Config{})
	noNet := noNetworkSpec("https://wardynd.wardyn-system.svc.cluster.local:8443")
	for _, tc := range []struct {
		name string
		set  func()
		spec runner.SandboxSpec
		ns   string
		want string
	}{
		{"not proven", func() { setVerdict(d, netPolNotProven) }, noNet, testNamespace, "netpol_not_proven"},
		{"no verdict for the namespace", func() { setVerdict(d, netPolProven) }, noNet, "other", "netpol_not_proven"},
		{"a state with no name", func() { setVerdict(d, netPolState(99)) }, noNet, testNamespace, "netpol_not_proven"},
		{"not a No network run", func() { setVerdict(d, netPolUnenforced) }, testSandboxSpec(), testNamespace, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set()
			if got := d.noNetworkProxyOnly(tc.spec, tc.ns); got != tc.want {
				t.Errorf("noNetworkProxyOnly = %q, want %q", got, tc.want)
			}
		})
	}
}
