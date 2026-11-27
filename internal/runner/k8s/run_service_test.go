// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyRead is one scripted read of the run's proxy pod.
type proxyRead struct {
	ip    string
	ips   []string // Status.PodIPs; nil leaves it empty
	ready bool
}

// eventLog records, in order, what the reactors of one test saw.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) index(e string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Index(l.events, e)
}

// scriptProxyReads answers the n-th read of the run's proxy pod with reads[n], the last one from then on, and logs
// "agent-create", "slice-update" and "proxy-ready-read" as each happens.
func scriptProxyReads(cs *fake.Clientset, runID uuid.UUID, reads ...proxyRead) *eventLog {
	log := &eventLog{}
	n := 0
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.(clienttesting.GetAction).GetName() != proxyPodName(runID) {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, a.GetNamespace(), proxyPodName(runID))
		if err != nil {
			return true, nil, err
		}
		r := reads[min(n, len(reads)-1)]
		n++
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.PodIP, pod.Status.Phase = r.ip, corev1.PodPending
		for _, ip := range r.ips {
			pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
		}
		state := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		if r.ready {
			pod.Status.Phase = corev1.PodRunning
			state = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			log.add("proxy-ready-read")
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: proxyContainerName, State: state, Ready: r.ready}}
		return true, pod, nil
	})
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && pod.Name == agentPodName(runID) {
			log.add("agent-create")
		}
		return false, nil, nil
	})
	cs.PrependReactor("update", "endpointslices", func(clienttesting.Action) (bool, runtime.Object, error) {
		log.add("slice-update")
		return false, nil, nil
	})
	return log
}

// serverRun is a server-mode driver with the agent scripted Running and the proxy scripted by reads.
func serverRun(t *testing.T, cfg Config, reads ...proxyRead) (*Driver, *fake.Clientset, runner.SandboxSpec, *eventLog) {
	t.Helper()
	d, cs := newTestDriver(t, cfg)
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	log := scriptProxyReads(cs, spec.RunID, reads...)
	cs.ClearActions()
	return d, cs, spec, log
}

var readyAt7 = proxyRead{ip: "10.244.0.7", ready: true}

// writes is every action but a read, as "<verb> <resource>".
func writes(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		switch a.GetVerb() {
		case "get", "list", "watch":
			continue
		}
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
	}
	return out
}

// sliceAddressesWritten is the addresses of every EndpointSlice create and update the fake received, one entry per
// write ("" for a write that names no endpoint).
func sliceAddressesWritten(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		w, ok := a.(interface{ GetObject() runtime.Object })
		if !ok || a.GetResource().Resource != "endpointslices" {
			continue
		}
		es, ok := w.GetObject().(*discoveryv1.EndpointSlice)
		if !ok {
			continue
		}
		ips := sliceIPs(es)
		if len(ips) == 0 {
			out = append(out, "")
		}
		out = append(out, ips...)
	}
	return out
}

// assertRunAddressGone checks that no Service or EndpointSlice of the run is left.
func assertRunAddressGone(t *testing.T, cs *fake.Clientset, runID uuid.UUID) {
	t.Helper()
	if _, err := cs.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "services"}, testNamespace, keepServiceName(runID)); !isNotFound(err) {
		t.Errorf("the run's Service is still there (get err %v)", err)
	}
	list, err := cs.DiscoveryV1().EndpointSlices(testNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()})
	if err != nil {
		t.Fatalf("list EndpointSlices: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d EndpointSlice(s) of the run left behind", len(list.Items))
	}
}

func envValue(pod *corev1.Pod, name string) (string, bool) {
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func agentPod(t *testing.T, cs *fake.Clientset, runID uuid.UUID) *corev1.Pod {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), agentPodName(runID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	return pod
}

func TestRunService_ServerAgentPinsTheServiceAddress(t *testing.T) {
	d, cs, spec, _ := serverRun(t, Config{}, readyAt7)
	installPodUIDReactor(cs) // the fake gives no UID, and the owner reference needs the proxy pod's
	spec.Interactive = true
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod := agentPod(t, cs, spec.RunID)
	if ha := pod.Spec.HostAliases; len(ha) != 1 || ha[0].IP != "10.96.7.7" {
		t.Errorf("hostAliases = %+v, want the Service's ClusterIP 10.96.7.7", ha)
	}
	if dns := pod.Spec.DNSConfig; dns == nil || !slices.Equal(dns.Nameservers, []string{"10.96.7.7"}) {
		t.Errorf("DNSConfig = %+v, want the Service's ClusterIP as the one nameserver", dns)
	}
	if got := sliceIPs(keepSlice(t, cs, spec.RunID)); !slices.Equal(got, []string{"10.244.0.7"}) {
		t.Errorf("EndpointSlice endpoints = %v, want the proxy pod's IP", got)
	}
	if o := pod.OwnerReferences; len(o) != 1 || o[0].Kind != "Pod" || o[0].Name != proxyPodName(spec.RunID) {
		t.Errorf("agent owners = %+v, want the proxy pod", o)
	}
	want := strconv.Itoa(int((d.cfg.StartTimeout+d.cfg.CapacityWait)/time.Second) + 1)
	if got, ok := envValue(pod, proxyWaitSecsEnv); !ok || got != want {
		t.Errorf("interactive %s = %q (set %v), want %q", proxyWaitSecsEnv, got, ok, want)
	}

	d2, cs2, exec, _ := serverRun(t, Config{}, readyAt7)
	if _, err := d2.CreateSandbox(context.Background(), exec); err != nil {
		t.Fatalf("CreateSandbox (exec run): %v", err)
	}
	if got, ok := envValue(agentPod(t, cs2, exec.RunID), proxyWaitSecsEnv); ok {
		t.Errorf("exec run carries %s = %q, want it absent", proxyWaitSecsEnv, got)
	}

	d3, cs3, own, _ := serverRun(t, Config{}, readyAt7)
	own.Interactive = true
	own.Env[proxyWaitSecsEnv] = "42"
	if _, err := d3.CreateSandbox(context.Background(), own); err != nil {
		t.Fatalf("CreateSandbox (own value): %v", err)
	}
	if got, _ := envValue(agentPod(t, cs3, own.RunID), proxyWaitSecsEnv); got != "42" {
		t.Errorf("a run's own %s = %q, want its 42 kept", proxyWaitSecsEnv, got)
	}
}

func TestRunService_AgentCreatedBeforeTheProxyIsReady(t *testing.T) {
	pending := proxyRead{ip: "10.244.0.7"}
	d, _, spec, log := serverRun(t, Config{}, pending, pending, pending, readyAt7)
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	created, ready := log.index("agent-create"), log.index("proxy-ready-read")
	if created < 0 || ready < 0 || created > ready {
		t.Errorf("events = %v: the agent pod must be created before the first read showing the proxy Ready", log.events)
	}
}

func TestRunService_ActionOrder(t *testing.T) {
	pending := proxyRead{ip: "10.244.0.7"}
	d, cs, spec, log := serverRun(t, Config{}, pending, pending, pending, readyAt7)
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	want := []string{
		"create networkpolicies", "create networkpolicies", "create secrets", "create services", "create endpointslices",
		"create pods", "create pods", "update endpointslices",
	}
	if got := writes(cs); !slices.Equal(got, want) {
		t.Errorf("writes = %v\nwant %v", got, want)
	}
	if got := sliceAddressesWritten(cs); !slices.Equal(got, []string{"", "10.244.0.7"}) {
		t.Errorf("slice writes = %v, want an empty create, then one update naming the proxy's IP", got)
	}
	if u, r := log.index("slice-update"), log.index("proxy-ready-read"); u < 0 || r < 0 || u > r {
		t.Errorf("events = %v: the slice must name the proxy before the proxy reads Ready", log.events)
	}
}

func TestRunService_ChangedAddressIsWrittenAgain(t *testing.T) {
	d, cs, spec, _ := serverRun(t, Config{},
		proxyRead{ip: "10.244.0.7"}, proxyRead{ip: "10.244.0.8"}, proxyRead{ip: "10.244.0.8"}, proxyRead{ip: "10.244.0.8", ready: true})
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got := sliceAddressesWritten(cs); !slices.Equal(got, []string{"", "10.244.0.7", "10.244.0.8"}) {
		t.Errorf("slice writes = %v, want an empty create, the first IP, then the new one", got)
	}
	if got := sliceIPs(keepSlice(t, cs, spec.RunID)); !slices.Equal(got, []string{"10.244.0.8"}) {
		t.Errorf("EndpointSlice endpoints = %v, want the proxy's address at Ready", got)
	}
}

func TestRunService_ProxyNeverReadyRollsEverythingBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		read    proxyRead
		wantErr string
	}{
		{"image_pull_backoff", proxyRead{ip: "10.244.0.7"}, "ImagePullBackOff"},
		{"deadline", proxyRead{ip: "10.244.0.7"}, "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, spec, _ := serverRun(t, Config{}, tc.read)
			if tc.name == "image_pull_backoff" {
				cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if a.(clienttesting.GetAction).GetName() != proxyPodName(spec.RunID) {
						return false, nil, nil
					}
					obj, err := cs.Tracker().Get(podsGVR, a.GetNamespace(), proxyPodName(spec.RunID))
					if err != nil {
						return true, nil, err
					}
					pod := obj.(*corev1.Pod).DeepCopy()
					pod.Status.PodIP, pod.Status.Phase = "10.244.0.7", corev1.PodPending
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: proxyContainerName,
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "simulated"}}}}
					return true, pod, nil
				})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, err := d.CreateSandbox(ctx, spec)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CreateSandbox = %v, want an error naming %q", err, tc.wantErr)
			}
			if !slices.Contains(writes(cs), "create pods") || agentCreates(cs, spec.RunID) != 1 {
				t.Errorf("the agent pod was not created beside its proxy: writes %v", writes(cs))
			}
			var deletes []string
			for _, w := range writes(cs) {
				if strings.HasPrefix(w, "delete") {
					deletes = append(deletes, w)
				}
			}
			want := []string{"delete-collection pods", "delete-collection secrets", "delete services", "delete-collection endpointslices", "delete-collection networkpolicies"}
			if !slices.Equal(deletes, want) {
				t.Errorf("rollback deletes = %v\nwant %v", deletes, want)
			}
			assertRunObjectsGone(t, cs, spec.RunID)
			assertRunAddressGone(t, cs, spec.RunID)
		})
	}
}

func agentCreates(cs *fake.Clientset, runID uuid.UUID) int {
	n := 0
	for _, a := range cs.Actions() {
		if ca, ok := a.(clienttesting.CreateAction); ok && a.GetResource().Resource == "pods" {
			if pod, ok := ca.GetObject().(*corev1.Pod); ok && pod.Name == agentPodName(runID) {
				n++
			}
		}
	}
	return n
}

func TestRunService_TeardownDeletesTheServiceBetweenSecretAndPolicies(t *testing.T) {
	d, cs, spec, _ := serverRun(t, Config{}, readyAt7)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	cs.ClearActions()
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	want := []string{"delete-collection pods", "delete-collection secrets", "delete services", "delete-collection endpointslices", "delete-collection networkpolicies"}
	if got := writes(cs); !slices.Equal(got, want) {
		t.Errorf("teardown writes = %v\nwant %v", got, want)
	}
	podsWaited, secretsGone := false, false
	for _, a := range cs.Actions() {
		switch {
		case a.GetResource().Resource == "pods" && (a.GetVerb() == "list" || a.GetVerb() == "get"):
			podsWaited = true
		case a.GetVerb() == "delete-collection" && a.GetResource().Resource == "secrets":
			secretsGone = true
		case a.GetVerb() == "delete" && a.GetResource().Resource == "services":
			if !podsWaited || !secretsGone {
				t.Errorf("the Service was deleted before the pods' wait (%v) or the Secret delete (%v)", podsWaited, secretsGone)
			}
			if name := a.(clienttesting.DeleteAction).GetName(); name != keepServiceName(spec.RunID) {
				t.Errorf("deleted Service %q, want %q", name, keepServiceName(spec.RunID))
			}
		case a.GetVerb() == "delete-collection" && a.GetResource().Resource == "endpointslices":
			if sel := a.(clienttesting.DeleteCollectionAction).GetListRestrictions().Labels.String(); sel != labelRun+"="+spec.RunID.String() {
				t.Errorf("EndpointSlice delete-collection selector = %q, want the run's label", sel)
			}
		}
	}
	assertRunObjectsGone(t, cs, spec.RunID)
	assertRunAddressGone(t, cs, spec.RunID)
}

// failServiceDelete makes the run's Service delete, or its EndpointSlice delete-collection, answer err while on is set.
func failServiceDelete(cs *fake.Clientset, verb, resource string, err error) *atomic.Bool {
	on := &atomic.Bool{}
	on.Store(true)
	cs.PrependReactor(verb, resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		if on.Load() {
			return true, nil, err
		}
		return false, nil, nil
	})
	return on
}

// teardownVerbs reports whether the Secret and NetworkPolicy delete-collections ran.
func teardownVerbs(cs *fake.Clientset) (secrets, policies bool) {
	for _, a := range cs.Actions() {
		if a.GetVerb() != "delete-collection" {
			continue
		}
		switch a.GetResource().Resource {
		case "secrets":
			secrets = true
		case "networkpolicies":
			policies = true
		}
	}
	return secrets, policies
}

// createForTeardown makes a run with keep off (newTestDriver) or on (keepRig with a kept run) and clears the actions.
func createForTeardown(t *testing.T, keep bool) (*Driver, *fake.Clientset, uuid.UUID, string) {
	t.Helper()
	if keep {
		d, cs, _ := keepRig(t, Config{AgentSandboxKeep: true})
		sb, err := d.CreateSandbox(context.Background(), keptSpec())
		if err != nil {
			t.Fatalf("CreateSandbox (keep on): %v", err)
		}
		cs.ClearActions()
		return d, cs, asRunID, sb.Ref
	}
	d, cs, spec, _ := serverRun(t, Config{}, readyAt7)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox (keep off): %v", err)
	}
	cs.ClearActions()
	return d, cs, spec.RunID, sb.Ref
}

func TestRunService_ForbiddenServiceDelete(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", errors.New("test: the Role lacks services delete"))
	for _, keep := range []bool{false, true} {
		t.Run("keep="+strconv.FormatBool(keep), func(t *testing.T) {
			d, cs, _, ref := createForTeardown(t, keep)
			failServiceDelete(cs, "delete", "services", forbidden)
			err := d.KillSandbox(context.Background(), ref)
			secrets, policies := teardownVerbs(cs)
			if !secrets {
				t.Error("the Secret delete-collection did not run")
			}
			if keep {
				if err == nil || !apierrors.IsForbidden(err) || policies {
					t.Errorf("keep on: KillSandbox = %v, NetworkPolicies deleted %v; want the 403 returned and the policies kept", err, policies)
				}
				return
			}
			// The Service is still there and readable, so the NetworkPolicies stay for the sweep.
			if err == nil || !apierrors.IsForbidden(err) || policies {
				t.Errorf("keep off, Service readable: KillSandbox = %v, NetworkPolicies deleted %v; want the 403 returned and the policies kept", err, policies)
			}
		})
	}
}

// With keep off, a 403 is tolerated only where the Service reads as gone or cannot be read at all: a Role of an older
// release with no per-run Service verb, or a slice delete refused after the Service went.
func TestRunService_ForbiddenDeleteToleratedOnlyWithNoServiceLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse [][2]string // verb, resource answered 403
	}{
		{"role without service verbs", [][2]string{{"delete", "services"}, {"get", "services"}}},
		{"slice delete after the service went", [][2]string{{"delete-collection", "endpointslices"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, _, ref := createForTeardown(t, false)
			for _, r := range tc.refuse {
				failServiceDelete(cs, r[0], r[1], apierrors.NewForbidden(schema.GroupResource{Resource: r[1]}, "", errors.New("test: refused")))
			}
			err := d.KillSandbox(context.Background(), ref)
			secrets, policies := teardownVerbs(cs)
			if err != nil || !secrets || !policies {
				t.Errorf("KillSandbox = %v, Secret deleted %v, NetworkPolicies deleted %v; want nil and both deleted", err, secrets, policies)
			}
		})
	}
}

func TestRunService_FailedServiceDeleteKeepsPolicies(t *testing.T) {
	for _, target := range []struct{ verb, resource string }{{"delete", "services"}, {"delete-collection", "endpointslices"}} {
		for _, keep := range []bool{false, true} {
			t.Run(target.resource+"/keep="+strconv.FormatBool(keep), func(t *testing.T) {
				d, cs, runID, ref := createForTeardown(t, keep)
				on := failServiceDelete(cs, target.verb, target.resource, apierrors.NewInternalError(errors.New("test: the API server failed")))
				if err := d.KillSandbox(context.Background(), ref); err == nil {
					t.Fatal("KillSandbox = nil, want the 500 returned")
				}
				secrets, policies := teardownVerbs(cs)
				if !secrets || policies {
					t.Errorf("Secret delete ran %v, NetworkPolicy delete ran %v; want the Secret deleted and the policies kept", secrets, policies)
				}
				// The orphan sweep finds the run by its NetworkPolicies and tears it down again.
				on.Store(false)
				cs.ClearActions()
				zero := int64(0)
				if err := d.teardownByRunID(context.Background(), runID, &zero); err != nil {
					t.Fatalf("second teardown: %v", err)
				}
				if _, policies := teardownVerbs(cs); !policies {
					t.Error("the second teardown did not delete the NetworkPolicies")
				}
				assertRunAddressGone(t, cs, runID)
			})
		}
	}
}

func TestRunService_RunnerModeMakesNoService(t *testing.T) {
	px := httptest.NewServer(http.NotFoundHandler())
	defer px.Close()
	h := newRunnerHarness(t)
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
	installAgentRunningReactor(t, h.cs)
	spec := testSandboxSpec()
	spec.RunID = h.runID
	spec.Interactive = true
	h.cs.ClearActions()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var creates []string
	for _, a := range h.cs.Actions() {
		switch r := a.GetResource().Resource; r {
		case "services", "endpointslices":
			t.Errorf("runner mode issued %s %s", a.GetVerb(), r)
		case "networkpolicies", "secrets", "pods":
			if a.GetVerb() == "create" {
				creates = append(creates, r)
			}
		}
	}
	if want := []string{"networkpolicies", "networkpolicies", "secrets", "pods", "pods"}; !slices.Equal(creates, want) {
		t.Errorf("runner create order = %v, want %v", creates, want)
	}
	pods, err := h.cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if p.Name != agentPodName(spec.RunID) {
			continue
		}
		if ha := p.Spec.HostAliases; len(ha) != 1 || ha[0].IP != "10.244.0.7" {
			t.Errorf("runner agent hostAliases = %+v, want the proxy pod's IP", ha)
		}
		if _, ok := envValue(&p, proxyWaitSecsEnv); ok {
			t.Errorf("runner agent carries %s, want it absent", proxyWaitSecsEnv)
		}
	}
}

func TestRunService_EveryCallIsInRunAddressAccess(t *testing.T) {
	allowed := map[string]bool{}
	for _, a := range runAddressAccess {
		allowed[a.Resource+" "+a.Verb] = true
	}
	check := func(t *testing.T, path string, cs *fake.Clientset) {
		t.Helper()
		for _, a := range cs.Actions() {
			r := a.GetResource().Resource
			if r != "services" && r != "endpointslices" {
				continue
			}
			verb := a.GetVerb()
			if verb == "delete-collection" {
				verb = "deletecollection"
			}
			if r == "endpointslices" && verb == "delete" {
				t.Errorf("%s: a single EndpointSlice delete, which no Role grants", path)
			}
			if !allowed[r+" "+verb] {
				t.Errorf("%s: %s %s is not in runAddressAccess", path, verb, r)
			}
		}
	}

	// A server run with keep off: create and teardown.
	d, cs, spec, _ := serverRun(t, Config{}, readyAt7)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.KillSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	check(t, "keep off", cs)

	// A kept run: create, end and revive.
	kd, kcs, _ := keepRig(t, Config{AgentSandboxKeep: true})
	ksb, err := kd.CreateSandbox(context.Background(), claudeKeptSpec())
	if err != nil {
		t.Fatalf("CreateSandbox (kept): %v", err)
	}
	check(t, "kept create", kcs)
	k := &keepDriver{Driver: kd}
	if err := k.StopProxy(context.Background(), ksb.Ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("StopProxy on a server = %v, want ErrEndUnsupported", err)
	}
	if err := k.EndSandbox(context.Background(), ksb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	check(t, "kept end", kcs)
	k2, rcs, _, ref := keptRun(t)
	if err := k2.ReplaceProxy(context.Background(), ref, replaceCfg); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	check(t, "kept revive", rcs)
}

func TestRunService_KeptRecordSurvivesTheAddressWrite(t *testing.T) {
	d, cs, _ := keepRig(t, Config{AgentSandboxKeep: true})
	if _, err := d.CreateSandbox(context.Background(), claudeKeptSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	recorded, addressAfter := false, false
	for _, a := range cs.Actions() {
		ua, ok := a.(clienttesting.UpdateAction)
		if !ok || a.GetResource().Resource != "endpointslices" {
			continue
		}
		es := ua.GetObject().(*discoveryv1.EndpointSlice)
		kept := es.Annotations[annotationKeptSandbox] == string(asSandboxUID)
		if kept && len(sliceIPs(es)) == 0 {
			recorded = true
		}
		if recorded && kept && slices.Equal(sliceIPs(es), []string{"10.244.0.7"}) {
			addressAfter = true
		}
	}
	if !recorded || !addressAfter {
		t.Errorf("recorded %v, then an address write keeping the record %v; want both", recorded, addressAfter)
	}
	es := keepSlice(t, cs, asRunID)
	if es.Annotations[annotationKeptSandbox] != string(asSandboxUID) || !slices.Equal(sliceIPs(es), []string{"10.244.0.7"}) {
		t.Errorf("slice = %v annotations %v, want the proxy IP and the kept Sandbox's UID", sliceIPs(es), es.Annotations)
	}
}

func TestRunService_ProxyWaitEnvNeverMutates(t *testing.T) {
	server := &Driver{cfg: Config{StartTimeout: time.Minute, CapacityWait: time.Minute}}
	env := map[string]string{"A": "1"}
	got := server.proxyWaitEnv(runner.SandboxSpec{Interactive: true, Env: env})
	if _, ok := env[proxyWaitSecsEnv]; ok || len(env) != 1 {
		t.Errorf("caller's map = %v, want it unchanged", env)
	}
	if got[proxyWaitSecsEnv] != "121" || got["A"] != "1" {
		t.Errorf("proxyWaitEnv = %v, want A kept and %s=121", got, proxyWaitSecsEnv)
	}
	if got := server.proxyWaitEnv(runner.SandboxSpec{Interactive: true}); got[proxyWaitSecsEnv] != "121" {
		t.Errorf("proxyWaitEnv with no env = %v, want %s=121", got, proxyWaitSecsEnv)
	}
	runnerD := &Driver{cfg: Config{RunnerID: "runner-1", StartTimeout: time.Minute}}
	got = runnerD.proxyWaitEnv(runner.SandboxSpec{Interactive: true, Env: env})
	if len(got) != 1 || got["A"] != "1" {
		t.Errorf("runner mode proxyWaitEnv = %v, want spec.Env unchanged", got)
	}
	got["B"] = "2" // it is spec.Env itself, not a copy
	if env["B"] != "2" {
		t.Error("runner mode returned a copy, want spec.Env itself")
	}
}

func TestRunService_DualStackPicksTheServiceFamily(t *testing.T) {
	t.Run("has_the_family", func(t *testing.T) {
		d, cs, spec, _ := serverRun(t, Config{}, proxyRead{ip: "10.244.0.7", ips: []string{"10.244.0.7", "fd00:10:244::7"}, ready: true})
		installKeepServiceReactor(cs, "fd00:10:96::7")
		if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		es := keepSlice(t, cs, spec.RunID)
		if es.AddressType != discoveryv1.AddressTypeIPv6 || !slices.Equal(sliceIPs(es), []string{"fd00:10:244::7"}) {
			t.Errorf("slice %s %v, want IPv6 naming fd00:10:244::7", es.AddressType, sliceIPs(es))
		}
	})
	t.Run("lacks_the_family", func(t *testing.T) {
		d, cs, spec, _ := serverRun(t, Config{}, proxyRead{ip: "10.244.0.7", ips: []string{"10.244.0.7"}, ready: true})
		installKeepServiceReactor(cs, "fd00:10:96::7")
		_, err := d.CreateSandbox(context.Background(), spec)
		if err == nil || !strings.Contains(err.Error(), "IPv6") {
			t.Fatalf("CreateSandbox = %v, want a refusal naming the IPv6 family", err)
		}
		assertRunObjectsGone(t, cs, spec.RunID)
		assertRunAddressGone(t, cs, spec.RunID)
	})
	pod := &corev1.Pod{Status: corev1.PodStatus{PodIP: "10.244.0.9"}}
	if got := podIPOfFamily(pod, discoveryv1.AddressTypeIPv6); got != "10.244.0.9" {
		t.Errorf("podIPOfFamily with no PodIPs = %q, want PodIP", got)
	}
}
