// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestCheckAgentSandboxKeepNeedsTheBackend(t *testing.T) {
	err := checkAgentSandboxKeep(Config{AgentSandboxKeep: true})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_K8S_AGENT_SANDBOX_KEEP needs WARDYN_K8S_AGENT_SANDBOX on") {
		t.Fatalf("keep without the backend: err = %v, want the refusal", err)
	}
	for _, cfg := range []Config{{}, {AgentSandbox: true}, {AgentSandbox: true, AgentSandboxKeep: true}} {
		if err := checkAgentSandboxKeep(cfg); err != nil {
			t.Errorf("checkAgentSandboxKeep(%+v) = %v, want nil", cfg, err)
		}
	}
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX_KEEP", "true")
	if !buildConfig(substrate.Deps{}).AgentSandboxKeep {
		t.Error("buildConfig did not read WARDYN_K8S_AGENT_SANDBOX_KEEP")
	}
	// A runner refuses the switch alone with the keep message, and with the backend with the backend's.
	t.Setenv("WARDYN_K8S_NAMESPACE", "")
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "")
	if _, err := NewRunner(substrate.Deps{}, func(*Config) {}); err == nil || !strings.Contains(err.Error(), "WARDYN_K8S_AGENT_SANDBOX_KEEP needs") {
		t.Errorf("NewRunner with keep alone = %v, want the keep refusal", err)
	}
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "true")
	if _, err := NewRunner(substrate.Deps{}, func(*Config) {}); !errors.Is(err, errAgentSandboxRunner) {
		t.Errorf("NewRunner with keep and the backend = %v, want errAgentSandboxRunner", err)
	}
}

// Boot refuses a service account missing any verb the kept run's address needs, naming it.
func TestBootKeepChecksTheAddressVerbs(t *testing.T) {
	for _, tc := range []struct{ deny, want string }{
		{"create services", "needs create on services"},
		{"delete services", "needs delete on services"},
		{"update endpointslices", "needs update on endpointslices.discovery.k8s.io"},
		{"deletecollection endpointslices", "needs deletecollection on endpointslices.discovery.k8s.io"},
	} {
		t.Run(tc.deny, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactor(t, cs, false)
			cs.Resources = servedSandboxes()
			installAccessReactor(cs, tc.deny, nil)
			useDynamicClient(t, newFakeDynamic())
			cfg := Config{AgentSandbox: true, AgentSandboxKeep: true, Namespaces: []string{testNamespace}, ProxyImage: "p"}
			if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newWithClient err = %v, want a refusal containing %q", err, tc.want)
			}
			// With keep off the same denial does not matter.
			cfg.AgentSandboxKeep = false
			if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err != nil {
				t.Fatalf("keep off: newWithClient err = %v, want boot", err)
			}
		})
	}
}

func TestKeepRunNeedsInteractiveAndAKeptWorkFolder(t *testing.T) {
	work := types.DriveMount{Target: runner.ScratchWorkPath}
	other := types.DriveMount{Target: "/home/agent/other"}
	on := &Driver{cfg: Config{AgentSandbox: true, AgentSandboxKeep: true}}
	off := &Driver{cfg: Config{AgentSandbox: true}}
	for _, tc := range []struct {
		name string
		d    *Driver
		spec runner.SandboxSpec
		want bool
	}{
		{"keep off", off, runner.SandboxSpec{Interactive: true, Drives: []types.DriveMount{work}}, false},
		{"not interactive", on, runner.SandboxSpec{Drives: []types.DriveMount{work}}, false},
		{"no drive", on, runner.SandboxSpec{Interactive: true}, false},
		{"a drive at another target", on, runner.SandboxSpec{Interactive: true, Drives: []types.DriveMount{other}}, false},
		{"interactive with the work drive", on, runner.SandboxSpec{Interactive: true, Drives: []types.DriveMount{other, work}}, true},
	} {
		if got := tc.d.keepRun(tc.spec); got != tc.want {
			t.Errorf("%s: keepRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}

var endpointSlicesGVR = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}

// installKeepServiceReactor gives every Service create the ClusterIP and UID the API server would.
func installKeepServiceReactor(cs *fake.Clientset, clusterIP string) {
	cs.PrependReactor("create", "services", func(a clienttesting.Action) (bool, runtime.Object, error) {
		svc := a.(clienttesting.CreateAction).GetObject().(*corev1.Service)
		svc.Spec.ClusterIP, svc.UID = clusterIP, "service-uid-1"
		return false, nil, nil
	})
}

// installSliceFamilyReactor refuses an EndpointSlice update that changes the address type, as the API server does.
func installSliceFamilyReactor(cs *fake.Clientset) {
	cs.PrependReactor("update", "endpointslices", func(a clienttesting.Action) (bool, runtime.Object, error) {
		upd := a.(clienttesting.UpdateAction).GetObject().(*discoveryv1.EndpointSlice)
		stored, err := cs.Tracker().Get(endpointSlicesGVR, a.GetNamespace(), upd.Name)
		if err != nil {
			return true, nil, err
		}
		if stored.(*discoveryv1.EndpointSlice).AddressType != upd.AddressType {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Group: "discovery.k8s.io", Kind: "EndpointSlice"}, upd.Name,
				field.ErrorList{field.Invalid(field.NewPath("addressType"), upd.AddressType, "field is immutable")})
		}
		return false, nil, nil
	})
}

// keepOwner is a run's Service as the API server returns it, for the EndpointSlice tests.
func keepOwner(runID uuid.UUID, clusterIP string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: keepServiceName(runID), Namespace: testNamespace, UID: "service-uid-1", Labels: map[string]string{labelRun: runID.String()}},
		Spec:       corev1.ServiceSpec{ClusterIP: clusterIP},
	}
}

func keepSlice(t *testing.T, cs *fake.Clientset, runID uuid.UUID) *discoveryv1.EndpointSlice {
	t.Helper()
	es, err := cs.DiscoveryV1().EndpointSlices(testNamespace).Get(context.Background(), keepServiceName(runID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the run's EndpointSlice: %v", err)
	}
	return es
}

// sliceWrites counts the creates and updates the fake received on EndpointSlices.
func sliceWrites(cs *fake.Clientset, verb string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "endpointslices" {
			n++
		}
	}
	return n
}

func sliceIPs(es *discoveryv1.EndpointSlice) []string {
	var ips []string
	for _, e := range es.Endpoints {
		ips = append(ips, e.Addresses...)
	}
	return ips
}

func TestKeepServiceHasNoSelectorAndThreePorts(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installKeepServiceReactor(cs, "10.96.0.50")
	runID := uuid.New()
	svc, err := d.createKeepService(context.Background(), testNamespace, runID, map[string]string{labelRun: runID.String()})
	if err != nil {
		t.Fatalf("createKeepService: %v", err)
	}
	if svc.Name != keepServiceName(runID) || svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.Selector) != 0 || svc.Spec.ClusterIP != "10.96.0.50" {
		t.Errorf("Service = %+v, want a selectorless ClusterIP Service named for the run", svc)
	}
	type port struct {
		name  string
		proto corev1.Protocol
		port  int32
	}
	want := []port{{"proxy", corev1.ProtocolTCP, runner.ProxyListenPort}, {"dns-tcp", corev1.ProtocolTCP, 53}, {"dns-udp", corev1.ProtocolUDP, 53}}
	var got []port
	for _, p := range svc.Spec.Ports {
		got = append(got, port{p.Name, p.Protocol, p.Port})
		if p.TargetPort.IntVal != p.Port {
			t.Errorf("port %s targets %v, want its own port %d", p.Name, p.TargetPort, p.Port)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Service ports = %v, want %v", got, want)
	}

	// An API server that returns no ClusterIP is an error, not a Service with no address.
	d2, _ := newTestDriver(t, Config{})
	if _, err := d2.createKeepService(context.Background(), testNamespace, uuid.New(), nil); err == nil || !strings.Contains(err.Error(), "no ClusterIP") {
		t.Errorf("createKeepService without a ClusterIP: err = %v, want the refusal", err)
	}
}

func TestKeepEndpointsCreateThenRepointThenEmpty(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ctx, runID := context.Background(), uuid.New()
	owner := keepOwner(runID, "10.96.0.50")
	write := func(ip string) {
		t.Helper()
		if err := d.writeKeepEndpoints(ctx, testNamespace, runID, owner, ip); err != nil {
			t.Fatalf("writeKeepEndpoints(%q): %v", ip, err)
		}
	}
	write("10.244.0.7")
	es := keepSlice(t, cs, runID)
	if !slices.Equal(sliceIPs(es), []string{"10.244.0.7"}) || es.AddressType != discoveryv1.AddressTypeIPv4 || len(es.Ports) != 3 {
		t.Fatalf("created slice = %+v, want one IPv4 endpoint and three ports", es)
	}
	if es.Labels[discoveryv1.LabelServiceName] != owner.Name || es.Labels[discoveryv1.LabelManagedBy] != "wardyn.dev" || es.Labels[labelRun] != runID.String() {
		t.Errorf("slice labels = %v, want the service-name, managed-by and the owner's labels", es.Labels)
	}
	if len(es.OwnerReferences) != 1 || es.OwnerReferences[0].Kind != "Service" || es.OwnerReferences[0].UID != owner.UID {
		t.Errorf("slice owners = %+v, want the Service", es.OwnerReferences)
	}
	if e := es.Endpoints[0]; e.Conditions.Ready == nil || !*e.Conditions.Ready {
		t.Errorf("the endpoint is not ready: %+v", e)
	}

	write("10.244.0.9")
	if got := sliceIPs(keepSlice(t, cs, runID)); !slices.Equal(got, []string{"10.244.0.9"}) {
		t.Errorf("repointed endpoints = %v, want 10.244.0.9", got)
	}
	write("10.244.0.9")
	if n := sliceWrites(cs, "update"); n != 1 {
		t.Errorf("updates after a repeat of the same IP = %d, want 1: the repeat writes nothing", n)
	}
	write("")
	if got := sliceIPs(keepSlice(t, cs, runID)); len(got) != 0 {
		t.Errorf("emptied endpoints = %v, want none", got)
	}
	write("")
	if n, c := sliceWrites(cs, "update"), sliceWrites(cs, "create"); n != 2 || c != 1 {
		t.Errorf("slice writes = %d creates, %d updates, want 1 and 2: created once, updated in place", c, n)
	}
}

func TestKeepEndpointsIPv6EmptyKeepsTheFamily(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installSliceFamilyReactor(cs)
	ctx, runID := context.Background(), uuid.New()
	owner := keepOwner(runID, "fd00::50")
	for _, ip := range []string{"fd00::7", "", "fd00::8"} {
		if err := d.writeKeepEndpoints(ctx, testNamespace, runID, owner, ip); err != nil {
			t.Fatalf("writeKeepEndpoints(%q): %v", ip, err)
		}
		es := keepSlice(t, cs, runID)
		if es.AddressType != discoveryv1.AddressTypeIPv6 {
			t.Errorf("after %q the slice is %s, want IPv6", ip, es.AddressType)
		}
		if (ip == "") != (len(es.Endpoints) == 0) {
			t.Errorf("after %q the slice's endpoints = %v", ip, sliceIPs(es))
		}
	}
}

func TestKeepEndpointsEmptyCreateTakesTheServiceFamily(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installSliceFamilyReactor(cs)
	runID := uuid.New()
	if err := d.writeKeepEndpoints(context.Background(), testNamespace, runID, keepOwner(runID, "fd00::50"), ""); err != nil {
		t.Fatalf("writeKeepEndpoints: %v", err)
	}
	if es := keepSlice(t, cs, runID); es.AddressType != discoveryv1.AddressTypeIPv6 || len(es.Endpoints) != 0 {
		t.Errorf("slice = %s with %v, want IPv6 and no endpoint", es.AddressType, sliceIPs(es))
	}
}

func TestKeepEndpointsFamilyChangeIsRefused(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installSliceFamilyReactor(cs)
	ctx, runID := context.Background(), uuid.New()
	owner := keepOwner(runID, "10.96.0.50")
	if err := d.writeKeepEndpoints(ctx, testNamespace, runID, owner, "10.244.0.7"); err != nil {
		t.Fatalf("writeKeepEndpoints: %v", err)
	}
	err := d.writeKeepEndpoints(ctx, testNamespace, runID, owner, "fd00::9")
	if err == nil || !strings.Contains(err.Error(), "fd00::9 is not of the IPv4 family of the run's EndpointSlice") {
		t.Fatalf("a family change: err = %v, want the refusal", err)
	}
	if n := sliceWrites(cs, "update"); n != 0 {
		t.Errorf("updates = %d, want none: the refusal comes before any write", n)
	}
}

func TestKeepEndpointsKeepsAnnotations(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ctx, runID := context.Background(), uuid.New()
	owner := keepOwner(runID, "10.96.0.50")
	if err := d.writeKeepEndpoints(ctx, testNamespace, runID, owner, "10.244.0.7"); err != nil {
		t.Fatalf("writeKeepEndpoints: %v", err)
	}
	es := keepSlice(t, cs, runID)
	es.Annotations = map[string]string{"example.com/kept": "x"}
	if _, err := cs.DiscoveryV1().EndpointSlices(testNamespace).Update(ctx, es, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("annotate the slice: %v", err)
	}
	for _, ip := range []string{"10.244.0.8", ""} {
		if err := d.writeKeepEndpoints(ctx, testNamespace, runID, owner, ip); err != nil {
			t.Fatalf("writeKeepEndpoints(%q): %v", ip, err)
		}
		if got := keepSlice(t, cs, runID).Annotations["example.com/kept"]; got != "x" {
			t.Errorf("after writing %q the annotation = %q, want it kept", ip, got)
		}
	}
}

// keptSpec is a run that can be kept: interactive, with a managed drive as its work folder.
func keptSpec() runner.SandboxSpec {
	spec := asSpec()
	spec.Interactive = true
	drive := testDriveMount()
	drive.Target = runner.ScratchWorkPath
	spec.Drives = []types.DriveMount{*drive}
	return spec
}

func keepRig(t *testing.T, cfg Config) (*Driver, *fake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	d, cs, dyn := newSandboxRig(t, cfg, controllerMaking(t, nil))
	installKeepServiceReactor(cs, "10.96.0.50")
	denySecretReads(cs)
	cs.ClearActions()
	return d, cs, dyn
}

// denySecretReads refuses a get or a list of a Secret as the chart's Role does: it grants create, delete and
// deletecollection on Secrets, and no get, list or watch. A test reads a Secret through the tracker.
func denySecretReads(cs *fake.Clientset) {
	for _, verb := range []string{"get", "list"} {
		cs.PrependReactor(verb, "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("test: the Role grants no get, list or watch on secrets"))
		})
	}
}

func sandboxTemplate(t *testing.T, dyn *dynamicfake.FakeDynamicClient, ref string) (*unstructured.Unstructured, *corev1.Pod) {
	t.Helper()
	obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, ref)
	if err != nil {
		t.Fatalf("get the Sandbox: %v", err)
	}
	u := obj.(*unstructured.Unstructured)
	return u, podTemplateOf(t, u)
}

func TestCreateSandboxKeepPinsTheServiceAddress(t *testing.T) {
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	spec := keptSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	u, tmpl := sandboxTemplate(t, dyn, sb.Ref)
	if aliases := tmpl.Spec.HostAliases; len(aliases) != 1 || aliases[0].IP != "10.96.0.50" {
		t.Errorf("hostAliases = %+v, want wardyn-proxy at the Service's ClusterIP", aliases)
	}
	if dns := tmpl.Spec.DNSConfig; dns == nil || !slices.Equal(dns.Nameservers, []string{"10.96.0.50"}) {
		t.Errorf("DNSConfig = %+v, want the Service's ClusterIP as the one nameserver", dns)
	}
	owners := u.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "Service" || owners[0].Name != keepServiceName(spec.RunID) || owners[0].UID != "service-uid-1" {
		t.Errorf("Sandbox owners = %+v, want the run's Service", owners)
	}
	if tmpl.Labels[labelKeep] != "true" {
		t.Errorf("template labels = %v, want %s=true", tmpl.Labels, labelKeep)
	}
	if got := sliceIPs(keepSlice(t, cs, spec.RunID)); !slices.Equal(got, []string{"10.244.0.7"}) {
		t.Errorf("EndpointSlice endpoints = %v, want the proxy pod's IP 10.244.0.7", got)
	}
}

// assertNotKept is what a run that is not kept looks like with keep on or off: no Service or EndpointSlice is touched,
// the agent pins the proxy pod's IP, the proxy pod owns the agent, and there is no keep label.
func assertNotKept(t *testing.T, cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient, ref string) {
	t.Helper()
	for _, a := range cs.Actions() {
		if r := a.GetResource().Resource; r == "services" || r == "endpointslices" {
			t.Errorf("unexpected %s %s on a run that is not kept", a.GetVerb(), r)
		}
	}
	u, tmpl := sandboxTemplate(t, dyn, ref)
	if aliases := tmpl.Spec.HostAliases; len(aliases) != 1 || aliases[0].IP != "10.244.0.7" {
		t.Errorf("hostAliases = %+v, want the proxy pod's IP", aliases)
	}
	if owners := u.GetOwnerReferences(); len(owners) != 1 || owners[0].Kind != "Pod" || owners[0].Name != proxyPodName(asRunID) {
		t.Errorf("Sandbox owners = %+v, want the proxy pod", owners)
	}
	if _, ok := tmpl.Labels[labelKeep]; ok {
		t.Errorf("template labels = %v, want no %s", tmpl.Labels, labelKeep)
	}
}

func TestCreateSandboxKeepOnRunNotKeptUnchanged(t *testing.T) {
	for name, mutate := range map[string]func(*runner.SandboxSpec){
		"interactive with no drive": func(s *runner.SandboxSpec) { s.Drives = nil },
		"not interactive":           func(s *runner.SandboxSpec) { s.Interactive = false },
	} {
		t.Run(name, func(t *testing.T) {
			d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
			spec := keptSpec()
			mutate(&spec)
			sb, err := d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			assertNotKept(t, cs, dyn, sb.Ref)
		})
	}
}

func TestCreateSandboxKeepOffUnchanged(t *testing.T) {
	d, cs, dyn := keepRig(t, Config{})
	sb, err := d.CreateSandbox(context.Background(), keptSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	assertNotKept(t, cs, dyn, sb.Ref)
}

func TestTeardownKeepDeletesServiceAndSlices(t *testing.T) {
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	sb, err := d.CreateSandbox(context.Background(), keptSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	for name, run := range map[string]uuid.UUID{"run-slice": asRunID, "other-run-slice": uuid.New()} {
		es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{labelRun: run.String()}}, AddressType: discoveryv1.AddressTypeIPv4}
		if err := cs.Tracker().Create(endpointSlicesGVR, es, testNamespace); err != nil {
			t.Fatalf("seed EndpointSlice %s: %v", name, err)
		}
	}
	order := writesInOrder(cs, dyn)
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	want := []string{
		"delete sandboxes", "delete-collection pods", "delete services", "delete-collection endpointslices",
		"delete-collection secrets", "delete-collection networkpolicies",
	}
	if !slices.Equal(*order, want) {
		t.Errorf("teardown writes = %v\nwant %v", *order, want)
	}
	if _, err := cs.CoreV1().Services(testNamespace).Get(context.Background(), keepServiceName(asRunID), metav1.GetOptions{}); !isNotFound(err) {
		t.Errorf("the run's Service is still there (get err %v)", err)
	}
	list, err := cs.DiscoveryV1().EndpointSlices(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list EndpointSlices: %v", err)
	}
	var left []string
	for _, es := range list.Items {
		left = append(left, es.Name)
	}
	if want := []string{"other-run-slice"}; !slices.Equal(left, want) {
		t.Errorf("EndpointSlices after teardown = %v, want %v: this run's slice gone, another run's kept", left, want)
	}
}
