// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

// Package k6_test is lane K6's kind conformance suite for the Kubernetes runner (design K1 §8, and
// the K6 list in lanes/K2/review-fable.md). scripts/k6-kind-conformance.sh is its only caller: it
// creates the kind cluster, installs the runnerd chart, and runs this suite once per kubelet version.
//
// The suite drives the real k8s runner-mode Driver in process, as runnerd would, under runnerd's own
// ServiceAccount token (KUBECONFIG is a kubeconfig the script minted for the chart's rendered Role),
// never cluster-admin. The admin kubeconfig (K6_ADMIN_KUBECONFIG) plays every other party: the
// scheduler's timing (cordon, labels), a third party's finalizer, the node's owner.
//
// A "restart" is a second Driver over the same evidence store; a "crash" is a store view that
// persists one write and then refuses every later one, so the old Driver stops exactly after a
// durable step, as a killed process would.
//
// Each case prints one line, `K6-CASE <name> <status> <detail>`, which the script tabulates.
// Statuses: PASS, FAIL, INCONCLUSIVE (the cluster did not produce the condition the case needs;
// never a pass), and BLOCKED-ON-<lane> rows for items that need org-side behaviour not built yet.
package k6_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	schedLabel        = "wardyn-k6/sched" // the Driver's nodeSelector; the suite moves it between nodes
	stopProof         = "wardyn.dev/stop-proof"
	thirdPartyFinal   = "k6.wardyn.dev/third-party"
	labelRun          = "wardyn.run-id"
	labelComponent    = "wardyn.component"
	proxyComponent    = "proxy"
	agentComponent    = "agent"
	normalStopBound   = 20 * time.Second // §8 (R6-M1): a graceful StopProxy completes within 20 s
	createWait        = 3 * time.Minute
	goneWait          = 2 * time.Minute
	overlapIterations = 24
)

// env is what the script hands the suite.
type env struct {
	admin, sa                                        kubernetes.Interface
	runsNS, ownNS, saName, runnerID, relayIP, orgURL string
	orgCA, proxyImage, agentImage, arch, cluster     string
	peerLabels                                       map[string]string
	pin, other                                       string // the two workers: pin carries runs, other is spare
	nodes                                            []string
	store                                            *store
	sink                                             *fakeSink
	fences                                           *fences
	main                                             *k8s.Driver
	stoppedIP                                        string // normal-stop's proxy IP, for pod-ip-reuse-after-stop
}

func must(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s is required (scripts/k6-kind-conformance.sh sets it)", key)
	}
	return v
}

func TestK6(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K6") != "1" {
		t.Skip("WARDYN_TEST_K6=1 not set; run scripts/k6-kind-conformance.sh")
	}
	adminCfg, err := clientcmd.BuildConfigFromFlags("", must(t, "K6_ADMIN_KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	saCfg, err := clientcmd.BuildConfigFromFlags("", must(t, "KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		admin: kubernetes.NewForConfigOrDie(adminCfg), sa: kubernetes.NewForConfigOrDie(saCfg),
		runsNS: must(t, "K6_RUNS_NS"), ownNS: must(t, "K6_OWN_NS"), saName: must(t, "K6_SA"),
		runnerID: must(t, "K6_RUNNER_ID"), relayIP: must(t, "K6_RELAY_IP"), orgURL: must(t, "K6_ORG_URL"),
		proxyImage: must(t, "WARDYN_PROXY_IMAGE"), agentImage: must(t, "K6_AGENT_IMAGE"),
		arch: must(t, "K6_ARCH"), cluster: must(t, "K6_CLUSTER"),
		store: newStore(), fences: newFences(),
	}
	ca, err := os.ReadFile(must(t, "K6_ORG_CA"))
	if err != nil {
		t.Fatal(err)
	}
	e.orgCA = string(ca)
	if err := json.Unmarshal([]byte(must(t, "K6_RUNNERD_SELECTOR")), &e.peerLabels); err != nil {
		t.Fatalf("K6_RUNNERD_SELECTOR: %v", err)
	}
	e.sink = newFakeSink(e.relayIP)
	nodes, err := e.admin.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var workers []string
	for _, n := range nodes.Items {
		e.nodes = append(e.nodes, n.Name)
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; !cp {
			workers = append(workers, n.Name)
		}
	}
	sort.Strings(workers)
	if len(workers) < 2 {
		t.Fatalf("the suite needs two worker nodes, found %v", workers)
	}
	e.pin, e.other = workers[0], workers[1]

	var results []string
	kase := func(name string, fn func(t *testing.T, e *env)) {
		var status, detail string
		t.Run(name, func(t *testing.T) {
			defer func() {
				detail = notes(t)
				switch {
				case t.Failed():
					status = "FAIL"
				case t.Skipped():
					status = skipStatus(t)
				default:
					status = "PASS"
				}
			}()
			fn(t, e)
		})
		results = append(results, fmt.Sprintf("K6-CASE\t%s\t%s\t%s", name, status, detail))
	}

	// (Fable r8 L1) The IDs the guard compares must be distinct across kind nodes before any guard case.
	kase("node-identity-distinct", caseNodeIdentity)
	if t.Failed() {
		t.Fatal("kind node IDs are not distinct: every node guard below would be meaningless")
	}
	e.main = e.newDriver(t, e.store.view(nil))
	kase("rendered-role-boot-checks", caseBootChecks)
	kase("keystore-under-role", caseKeystore)
	kase("finalizer-api-semantics", caseFinalizerAPI)
	kase("normal-stop", caseNormalStop)
	kase("pod-ip-reuse-after-stop", casePodIPReuse)
	kase("h1-create-window", caseH1Window)
	kase("bind-delete-serial", caseBindDeleteSerial)
	kase("bind-delete-overlap", caseBindDeleteOverlap)
	kase("never-initialized-image-pull", caseNeverInitImagePull)
	kase("never-initialized-cancel-mid-pull", caseNeverInitCancel)
	kase("restart-while-unbound", caseRestartWhileUnbound)
	kase("crash-gaps-stop-proxy", caseCrashGapsStopProxy)
	kase("crash-gaps-label-teardown", caseCrashGapsTeardown)
	kase("crash-gaps-orphan-sweep", caseCrashGapsSweep)
	kase("hard-deny-in-proxy-config", caseHardDenyConfig)
	kase("retained-policy-probe", caseRetainedPolicy)
	kase("node-name-reuse-tick", caseNodeReuse) // last: it restarts a kind node
	for _, b := range blockedCases {
		results = append(results, fmt.Sprintf("K6-CASE\t%s\t%s\t%s", b[0], b[1], b[2]))
	}
	for _, r := range results {
		fmt.Println(r)
	}
}

// blockedCases are §8 / Fable items that need org-side behaviour no lane has built yet.
var blockedCases = [][3]string{
	{"fence-live-http2", "BLOCKED-ON-K3", "a renew and an activity report ride the relay to the org over a hub org session; the fake org has no hub"},
	{"self-lapse-end-to-end", "BLOCKED-ON-K3", "the lapse needs a renew refused through the relay (org session) and the lapsed frame on runnerd's real sink, which only runs for org-leased runs"},
	{"hard-deny-from-agent", "BLOCKED-ON-K3", "the proxy stays closed until its first renew through the org, so an agent request cannot reach the hard-deny check; the config half is hard-deny-in-proxy-config"},
	{"restart-fence-replay-reregister-sinks", "BLOCKED-ON-K3", "needs runnerd's own sink and relay holding a live proxy, i.e. an org-leased run"},
	{"org-triggered-sweep-rollback", "BLOCKED-ON-K3", "the sweep is dispatched by the org over the hub"},
	{"keystore-deleted-on-revoke", "BLOCKED-ON-K3", "revoke is an org message; keystore-under-role covers Set/Get/Delete under the Role"},
	{"mitm-inner-request-drain", "BLOCKED-ON-K3", "K9's open item (a closed client ends an in-flight MITM inner request inside the 3 s drain) needs a proxy serving agent traffic, which needs its first renew through an org session"},
	{"walk-trust-placement", "BLOCKED-ON-K4", "by_trust.org_cluster placement and the personal-runner refusal are K4's (needs RR)"},
}

// --- notes and statuses ------------------------------------------------------------------------

var (
	notesMu  sync.Mutex
	notesBy  = map[string][]string{}
	statusBy = map[string]string{}
)

func note(t *testing.T, format string, args ...any) {
	t.Helper()
	s := fmt.Sprintf(format, args...)
	t.Log(s)
	notesMu.Lock()
	notesBy[t.Name()] = append(notesBy[t.Name()], s)
	notesMu.Unlock()
}

func notes(t *testing.T) string {
	notesMu.Lock()
	defer notesMu.Unlock()
	return strings.Join(notesBy[t.Name()], "; ")
}

func inconclusive(t *testing.T, why string) {
	t.Helper()
	notesMu.Lock()
	statusBy[t.Name()] = "INCONCLUSIVE"
	notesMu.Unlock()
	note(t, "%s", why)
	t.SkipNow()
}

func skipStatus(t *testing.T) string {
	notesMu.Lock()
	defer notesMu.Unlock()
	if s := statusBy[t.Name()]; s != "" {
		return s
	}
	return "SKIP"
}

// --- evidence store with crash views ------------------------------------------------------------

var errCrashed = errors.New("k6: emulated runnerd crash (this store view is dead)")

type store struct {
	mu   sync.Mutex
	data map[uuid.UUID][]byte
}

func newStore() *store { return &store{data: map[uuid.UUID][]byte{}} }

// view is one runnerd process's handle on the durable store. crashAfter, when set, is asked after
// each persisted write; true kills the view, so the write is durable but the caller sees a failure
// and nothing that depends on it follows.
type view struct {
	s          *store
	dead       atomic.Bool
	crashAfter func(runID uuid.UUID, ev *evDoc) bool
}

func (s *store) view(crashAfter func(uuid.UUID, *evDoc) bool) *view {
	return &view{s: s, crashAfter: crashAfter}
}

func (v *view) LoadEvidence(id uuid.UUID) ([]byte, error) {
	if v.dead.Load() {
		return nil, errCrashed
	}
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	if b, ok := v.s.data[id]; ok {
		return append([]byte(nil), b...), nil
	}
	return nil, nil
}

func (v *view) SaveEvidence(id uuid.UUID, b []byte) error {
	if v.dead.Load() {
		return errCrashed
	}
	v.s.mu.Lock()
	v.s.data[id] = append([]byte(nil), b...)
	v.s.mu.Unlock()
	if v.crashAfter != nil {
		var ev evDoc
		if json.Unmarshal(b, &ev) == nil && v.crashAfter(id, &ev) {
			v.dead.Store(true)
			return errCrashed
		}
	}
	return nil
}

// evDoc is the part of the driver's per-run record the cases read (internal/runner/k8s/evidence.go).
type evDoc struct {
	Pods map[string]*struct {
		UID      string `json:"uid"`
		NodeName string `json:"node_name"`
		PodIP    string `json:"pod_ip"`
		Assoc    string `json:"assoc"`
		Before   any    `json:"before"`
		Witness  *struct {
			Arm        string `json:"arm"`
			Containers []struct {
				Name        string `json:"name"`
				ContainerID string `json:"container_id"`
				Tier        string `json:"tier"`
			} `json:"containers"`
		} `json:"witness"`
		FinalizerOff bool `json:"finalizer_off"`
	} `json:"pods"`
	Fence *struct {
		ProxyIP string `json:"proxy_ip"`
	} `json:"fence"`
	Reuse        []json.RawMessage `json:"reuse"`
	Hold         bool              `json:"hold"`
	ProducerGone bool              `json:"producer_gone"`
}

func (e *env) evidence(t *testing.T, id uuid.UUID) *evDoc {
	t.Helper()
	e.store.mu.Lock()
	b := e.store.data[id]
	e.store.mu.Unlock()
	ev := &evDoc{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, ev); err != nil {
			t.Fatalf("decode evidence of %s: %v", id, err)
		}
	}
	return ev
}

// arm returns the component's witness arm, "" without a witness.
func (ev *evDoc) arm(comp string) string {
	if p := ev.Pods[comp]; p != nil && p.Witness != nil {
		return p.Witness.Arm
	}
	return ""
}

// --- fake decision sink and relay fence ---------------------------------------------------------

type fakeSink struct {
	mu   sync.Mutex
	pin  string
	url  string
	gone map[uuid.UUID]int
}

func randHex() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newFakeSink(relayIP string) *fakeSink {
	return &fakeSink{pin: randHex(), url: "tls://" + relayIP + ":8444", gone: map[uuid.UUID]int{}}
}

func (s *fakeSink) NewKey() (string, [32]byte, error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", k, err
	}
	return hex.EncodeToString(k[:]), sha256.Sum256(k[:]), nil
}
func (s *fakeSink) Register(uuid.UUID, [32]byte) error     { return nil }
func (s *fakeSink) Drain(context.Context, uuid.UUID) error { return nil }
func (s *fakeSink) ProxyConfig(key string) any {
	return proxy.DecisionSinkConfig{URL: s.url, SPKISHA256: s.pin, Key: key}
}
func (s *fakeSink) ProducerGone(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	s.gone[id]++
	s.mu.Unlock()
	return nil
}
func (s *fakeSink) goneCount(id uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone[id]
}

// fences mirrors the relay's fence bookkeeping (cmd/wardyn-runnerd/relay_k8s.go): a fenced run's
// proxy IP is refused to every later run until the driver's Unfence releases it.
type fences struct {
	mu       sync.Mutex
	byRun    map[uuid.UUID]string
	released []uuid.UUID
	seen     map[uuid.UUID]string // every fence ever applied, released or not
}

func newFences() *fences { return &fences{byRun: map[uuid.UUID]string{}, seen: map[uuid.UUID]string{}} }

func (f *fences) fence(run uuid.UUID, ip string) {
	f.mu.Lock()
	f.byRun[run] = ip
	f.seen[run] = ip
	f.mu.Unlock()
}

func (f *fences) fenced(ip string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.byRun {
		if v == ip {
			return true
		}
	}
	return false
}

// unfence is the driver's release of a run's IP refusal at witness save (K2 round 3, Fable H2).
func (f *fences) unfence(run uuid.UUID, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byRun[run] == ip {
		delete(f.byRun, run)
		f.released = append(f.released, run)
	}
}

func (f *fences) wasReleased(run uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.released, run)
}

func (f *fences) ipOf(run uuid.UUID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ip, ok := f.seen[run]
	return ip, ok
}

// --- driver and runs ----------------------------------------------------------------------------

// newDriver builds a runner-mode Driver as runnerd's k8sRunnerHooks does, over v. Its boot canary
// runs on the pin node, so the pin node is labelled and schedulable first.
func (e *env) newDriver(t *testing.T, v *view) *k8s.Driver {
	t.Helper()
	e.setSched(t, e.pin)
	e.cordon(t, e.pin, false)
	hd, err := k8s.NewHardDeny("cluster.local", []string{e.relayIP}, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := k8s.NewRunner(substrate.Deps{ProxyImage: e.proxyImage}, func(c *k8s.Config) {
		c.Namespace = e.runsNS
		c.RunnerID = e.runnerID
		c.ControlPlaneRelay = func(*url.URL) (string, error) { return e.relayIP, nil }
		c.RunnerdPeer = &k8s.RunnerdPeer{Namespace: e.ownNS, PodLabels: e.peerLabels, RelayPort: 8443, SinkPort: 8444}
		c.Evidence = v
		c.Fence = e.fences.fence
		c.IPFenced = e.fences.fenced
		c.Sink = e.sink
		c.HardDeny = hd
		c.ReadNodes = true
		c.Arch = e.arch
		c.Unfence = e.fences.unfence
		c.StartTimeout = 90 * time.Second
		c.SandboxPlacement = fmt.Sprintf(`{"nodeSelector":{%q:"on"}}`, schedLabel)
	})
	if err != nil {
		t.Fatalf("k8s.NewRunner under the rendered Role: %v", err)
	}
	return d
}

func (e *env) spec(id uuid.UUID) runner.SandboxSpec {
	return runner.SandboxSpec{
		RunID: id, RunnerID: e.runnerID, Image: e.agentImage, ConfinementClass: types.CC1,
		Labels:      map[string]string{"wardyn.k6": "true"},
		ProxyConfig: runner.ProxyConfig{RunToken: "k6-" + id.String(), ControlPlaneURL: e.orgURL, ControlPlaneCAPEM: e.orgCA},
	}
}

// startRun creates a run on d in the background; the channel yields CreateSandbox's error.
func (e *env) startRun(ctx context.Context, d *k8s.Driver, id uuid.UUID) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := d.CreateSandbox(ctx, e.spec(id))
		ch <- err
	}()
	return ch
}

func waitErr(t *testing.T, ch <-chan error, within time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", what, within)
		return nil
	}
}

func ref(id uuid.UUID) string { return k8s.SandboxRef(id) }

func proxyName(id uuid.UUID) string { return "wardyn-proxy-" + id.String() }

// cleanupRun strips every finalizer from the run's pods and deletes what is left, so one case's
// leftovers never reach the next. It runs after a case's assertions.
func (e *env) cleanupRun(t *testing.T, id uuid.UUID) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sel := metav1.ListOptions{LabelSelector: labelRun + "=" + id.String()}
		pods, _ := e.admin.CoreV1().Pods(e.runsNS).List(ctx, sel)
		zero := int64(0)
		for _, p := range pods.Items {
			_, _ = e.admin.CoreV1().Pods(e.runsNS).Patch(ctx, p.Name, k8stypes.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
			_ = e.admin.CoreV1().Pods(e.runsNS).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		}
		_ = e.admin.NetworkingV1().NetworkPolicies(e.runsNS).DeleteCollection(ctx, metav1.DeleteOptions{}, sel)
		_ = e.admin.CoreV1().Secrets(e.runsNS).DeleteCollection(ctx, metav1.DeleteOptions{}, sel)
		_ = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			l, err := e.admin.CoreV1().Pods(e.runsNS).List(ctx, sel)
			return err == nil && len(l.Items) == 0, nil
		})
	})
}

// runGone reports whether every object of the run is gone.
func (e *env) runGone(ctx context.Context, id uuid.UUID) (bool, string) {
	sel := metav1.ListOptions{LabelSelector: labelRun + "=" + id.String()}
	pods, err1 := e.admin.CoreV1().Pods(e.runsNS).List(ctx, sel)
	pols, err2 := e.admin.NetworkingV1().NetworkPolicies(e.runsNS).List(ctx, sel)
	secs, err3 := e.admin.CoreV1().Secrets(e.runsNS).List(ctx, sel)
	if err := errors.Join(err1, err2, err3); err != nil {
		return false, err.Error()
	}
	left := fmt.Sprintf("pods=%d networkpolicies=%d secrets=%d", len(pods.Items), len(pols.Items), len(secs.Items))
	return len(pods.Items)+len(pols.Items)+len(secs.Items) == 0, left
}

func (e *env) waitRunGone(t *testing.T, id uuid.UUID, within time.Duration) {
	t.Helper()
	var left string
	err := poll(context.Background(), within, func(ctx context.Context) (bool, error) {
		var ok bool
		ok, left = e.runGone(ctx, id)
		return ok, nil
	})
	if err != nil {
		t.Fatalf("run %s objects not gone within %s: %s", id, within, left)
	}
}

func (e *env) podGone(t *testing.T, name string, within time.Duration) {
	t.Helper()
	err := poll(context.Background(), within, func(ctx context.Context) (bool, error) {
		_, err := e.admin.CoreV1().Pods(e.runsNS).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	if err != nil {
		t.Fatalf("pod %s still present after %s", name, within)
	}
}

func poll(ctx context.Context, within time.Duration, cond func(context.Context) (bool, error)) error {
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, within, true, cond)
}

// waitPod waits until the named pod satisfies cond.
func (e *env) waitPod(t *testing.T, name string, within time.Duration, what string, cond func(*corev1.Pod) bool) *corev1.Pod {
	t.Helper()
	var got *corev1.Pod
	err := poll(context.Background(), within, func(ctx context.Context) (bool, error) {
		p, err := e.admin.CoreV1().Pods(e.runsNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		got = p
		return cond(p), nil
	})
	if err != nil {
		t.Fatalf("pod %s: %s not reached within %s", name, what, within)
	}
	return got
}

// --- node helpers -------------------------------------------------------------------------------

// setSched puts the scheduling label on node only (none when node is "").
func (e *env) setSched(t *testing.T, node string) {
	t.Helper()
	for _, n := range e.nodes {
		val := "null"
		if n == node {
			val = `"on"`
		}
		e.patchNode(t, n, fmt.Sprintf(`{"metadata":{"labels":{%q:%s}}}`, schedLabel, val))
	}
}

func (e *env) cordon(t *testing.T, node string, on bool) {
	t.Helper()
	e.patchNode(t, node, fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, on))
}

func (e *env) patchNode(t *testing.T, node, patch string) {
	t.Helper()
	if _, err := e.admin.CoreV1().Nodes().Patch(context.Background(), node, k8stypes.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("patch node %s: %v", node, err)
	}
}

func (e *env) nodeUID(t *testing.T, node string) string {
	t.Helper()
	n, err := e.admin.CoreV1().Nodes().Get(context.Background(), node, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(n.UID)
}

// sh runs a host command (docker, kind) for the node-level cases.
func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// --- pod watch ----------------------------------------------------------------------------------

type podObs struct {
	node     string
	deleting bool
	phase    corev1.PodPhase
}

// watchProxy records every state of the run's proxy pod the apiserver sends, in order.
type podWatch struct {
	mu   sync.Mutex
	obs  []podObs
	last *corev1.Pod
	stop context.CancelFunc
}

func (e *env) watchProxy(t *testing.T, id uuid.UUID) *podWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w, err := e.admin.CoreV1().Pods(e.runsNS).Watch(ctx, metav1.ListOptions{LabelSelector: labelRun + "=" + id.String() + "," + labelComponent + "=" + proxyComponent})
	if err != nil {
		cancel()
		t.Fatalf("watch proxy pod: %v", err)
	}
	pw := &podWatch{stop: cancel}
	go func() {
		defer w.Stop()
		for ev := range w.ResultChan() {
			p, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			pw.mu.Lock()
			pw.obs = append(pw.obs, podObs{node: p.Spec.NodeName, deleting: p.DeletionTimestamp != nil, phase: p.Status.Phase})
			pw.last = p.DeepCopy()
			pw.mu.Unlock()
		}
	}()
	t.Cleanup(cancel)
	return pw
}

func (pw *podWatch) snapshot() ([]podObs, *corev1.Pod) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	return append([]podObs(nil), pw.obs...), pw.last
}

// classify names the winning order and reports a read showing deletionTimestamp without nodeName
// followed by a later nodeName (§8 r6 L4: never allowed).
func classify(obs []podObs) (order string, violation bool) {
	deletedUnbound := false
	for _, o := range obs {
		if o.deleting && o.node == "" {
			deletedUnbound = true
		}
		if deletedUnbound && o.node != "" {
			violation = true
		}
	}
	for _, o := range obs {
		if o.deleting {
			if o.node == "" {
				return "delete-first", violation
			}
			return "bind-first", violation
		}
		if o.node != "" {
			return "bind-first", violation
		}
	}
	return "unobserved", violation
}
