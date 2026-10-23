// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// conformance_k8s_test.go runs the conformance suite against the k8s driver,
// DIRECTLY against whatever kubeconfig context is current — no helm chart, no
// Postgres (mirrors conformance_docker_test.go's harness shape). Guarded by
// WARDYN_TEST_K8S=1; skipped cleanly when unset, so it never requires a live
// cluster in unit-CI.
//
// The //go:build k8s tag ensures this file only compiles under -tags k8s
// (matching the k8s driver's own build tag). ci.yml's conformance-k8s job
// stands up a kind cluster with Calico (NetworkPolicy enforcement — the
// substrate's boot-time canary REFUSES to construct without it — see
// internal/runner/k8s/canary.go) and runs `make test-conformance-k8s`.
//
// Agent image: WARDYN_TEST_K8S_AGENT_IMAGE — must be built from
// deploy/kind/Dockerfile.conformance-agent (busybox:1.36 + a real wardyn-rec
// binary; `make build-conformance-agent-image`), NOT bare busybox. The suite
// builds the substrate with Record: true, the production default (recording is
// off only under WARDYN_RECORDING_STORE=off) — a bare busybox agent image fails EVERY Exec closed
// (no wardyn-rec on PATH), so this suite would never reach a real verdict
// with one; review round 2 (H1) caught a first version of this file that
// silently took that failure as "conformance passed" via a since-reverted
// fallback in recordCmd itself. busybox:1.36 (not :latest): a `:latest` tag
// defaults to imagePullPolicy Always, which defeats `kind load` — the
// kubelet re-pulls over the network on every run regardless (M6).
// Proxy image: the REAL wardyn-proxy build, via WARDYN_PROXY_IMAGE — the
// construction-time canary launches it with -egress-canary instead of its
// normal entrypoint args, so a stand-in would make the gate vacuously
// refuse (ImagePullBackOff/exec-format-error, never a NetworkPolicy verdict)
// rather than actually proving anything.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

func TestConformanceK8s(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping k8s conformance")
	}

	proxyImage := os.Getenv("WARDYN_PROXY_IMAGE")
	if proxyImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE must name the REAL wardyn-proxy build (the boot-time canary launches it with -egress-canary; a stand-in image makes the gate meaningless — see this file's doc comment)")
	}
	agentImage := os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if agentImage == "" {
		t.Fatal("WARDYN_TEST_K8S_AGENT_IMAGE must name an image built from deploy/kind/Dockerfile.conformance-agent (`make build-conformance-agent-image`) — a recorder-less image (e.g. bare busybox) fails every Exec closed under this suite's Record: true, which would make the gate meaningless; see this file's doc comment")
	}

	// Namespace: the substrate's own real config knob (WARDYN_K8S_NAMESPACE),
	// resolved the same way register.go would — empty => k8s.New's own
	// "default" fallback (Config.withDefaults). ci.yml sets it to the
	// PSS-restricted namespace it labels.
	sub, err := k8s.New(k8s.Config{
		Namespace:  os.Getenv("WARDYN_K8S_NAMESPACE"),
		ProxyImage: proxyImage,
		Record:     true, // the production default; see the doc comment above
		// ConfinementRuntimes deliberately nil: this suite proves CC1 (the
		// unconditional floor); CC2/CC3 need a RuntimeClass pin this
		// throwaway conformance cluster does not provision (see A1's
		// Config.ConfinementRuntimes doc — a RuntimeClass name carries no
		// guessable platform convention).
	})
	if err != nil {
		// Construction itself IS the positive canary-direction proof: k8s.New
		// refuses to boot unless the boot-time egress canary proves this
		// cluster's CNI enforces NetworkPolicy (see internal/runner/k8s's
		// package doc). The negative direction (a kindnet-only cluster
		// correctly REFUSING) is proven out-of-band against a second
		// throwaway cluster — see the lane's report for that run's output;
		// internal/runner/k8s/canary_test.go already unit-tests both verdict
		// paths against a fake clientset.
		t.Fatalf("k8s.New: %v (this cluster's CNI must enforce NetworkPolicy — see ci.yml's Calico step)", err)
	}
	// Exercise the assembled production path: the orchestrator over the k8s
	// substrate is what the control plane actually runs.
	r := orchestrator.New(sub)
	cs, ns := conformanceClientset(t)

	conformance.Run(t, r, conformance.Options{
		SandboxImage: agentImage,
		Timeout:      3 * time.Minute,
		// The conformance-agent image's sh can exit with an explicit code;
		// the Wait conformance case Execs this and asserts Wait returns the
		// same code. It runs through recordCmd's wardyn-rec wrapper like
		// every other Exec on this substrate — see the file doc comment.
		ExitArgv: func(code int) []string {
			return []string{"sh", "-c", "exit " + strconv.Itoa(code)}
		},
		// EphemeralStorageProbe reads the ADMITTED pod back through `pods: get`
		// (no new RBAC verb — see deploy/helm/wardyn/templates/rbac.yaml) and
		// asserts the request the apiserver accepted is the small fixed floor.
		// This is the only assertion that can catch Kubernetes copying a LIMIT
		// into an unset request: the driver's fake-clientset tests can pin what
		// Wardyn SENDS, never what a real apiserver then defaults.
		EphemeralStorageProbe: ephemeralStorageProbe,
		UserDrives:            k8sDriveFixture(t, cs, ns, agentImage),
		// DefaultRouteProbe: nil (deliberately). This substrate declares
		// StructuralEgress:false — it proves L1 (NetworkPolicy), never L0
		// (see internal/runner/k8s's package doc) — so conformance.go's own
		// testL0StructuralEgress self-skips: there is no default route to
		// probe FOR. testAgentCannotReachAPIServer below is the L1
		// replacement.
	})

	t.Run("PlacementRouting", func(t *testing.T) {
		conformance.RunPlacementRouting(t, sub, conformance.RoutingOptions{Image: agentImage, Timeout: 3 * time.Minute})
	})

	t.Run("AgentCannotReachAPIServer", func(t *testing.T) {
		testAgentCannotReachAPIServer(t, r, agentImage)
	})

	t.Run("ExecIsAcceptedWithADiskBudget", func(t *testing.T) {
		testExecIsAcceptedWithADiskBudget(t, r, agentImage)
	})

	t.Run("AgentCannotReachAnythingButTheProxy", func(t *testing.T) {
		testAgentCannotReachAnythingButTheProxy(t, r, agentImage)
	})

	t.Run("ClosedOffAfterTeardown", func(t *testing.T) {
		testClosedOffAfterTeardown(t, r, cs, ns, agentImage)
	})
}

// heldScript holds a connection to the proxy and prints held=1 once it has stayed up for 2 s.
const heldScript = `sleep 3600 | nc wardyn-proxy 3128 &
pid=$!
sleep 2
kill -0 "$pid" && echo held=1
wait`

// closedOffBound is how long a held connection may outlive a teardown that has returned. A driver that
// waits for the pods to be gone has the kubelet's word that every container stopped, so this is only the
// beat the exec stream takes to notice. A driver that returns on a zero-grace delete leaves the
// containers running for the kubelet's own minimum grace (2 s) after the pod object is gone.
const closedOffBound = 500 * time.Millisecond

// leakyTeardown is the negative control: a driver whose Stop and Kill delete the run's pods with zero grace
// and return at once, without waiting for them to be gone. closedOff must fail it, or it proves nothing.
type leakyTeardown struct {
	runner.Runner
	cs kubernetes.Interface
	ns string
}

func (l leakyTeardown) KillSandbox(ctx context.Context, ref string) error { return l.drop(ctx, ref) }
func (l leakyTeardown) StopSandbox(ctx context.Context, ref string) error { return l.drop(ctx, ref) }

func (l leakyTeardown) drop(ctx context.Context, ref string) error {
	pod, err := l.cs.CoreV1().Pods(l.ns).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return l.cs.CoreV1().Pods(l.ns).DeleteCollection(ctx, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))},
		metav1.ListOptions{LabelSelector: "wardyn.run-id=" + pod.Labels["wardyn.run-id"]})
}

// testClosedOffAfterTeardown is the case a teardown that returns before the kubelet has stopped the pod's
// containers would pass every other case: once StopSandbox or KillSandbox returns, the run's pods are gone
// from the API and a connection the agent held to its proxy is closed. A pod IP probe from another pod
// cannot stand in for the second half here: the agent's NetworkPolicy denies all ingress, so a live pod and
// a dead one both stay silent to it.
//
// Non-vacuous by construction: the same check, run against leakyTeardown, must report an open pod or
// connection.
func testClosedOffAfterTeardown(t *testing.T, r runner.Runner, cs kubernetes.Interface, ns, agentImage string) {
	t.Helper()
	for _, tc := range []struct {
		name     string
		teardown func(runner.Runner) func(context.Context, string) error
	}{
		{"Kill", func(r runner.Runner) func(context.Context, string) error { return r.KillSandbox }},
		{"Stop", func(r runner.Runner) func(context.Context, string) error { return r.StopSandbox }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := closedOffAfter(t, r, cs, ns, agentImage, tc.teardown(r)); err != nil {
				t.Errorf("%s returned but the run is not closed off: %v", tc.name, err)
			}
		})
	}
	t.Run("ACheckThatPassesADriverWhichReturnsEarlyProvesNothing", func(t *testing.T) {
		leaky := leakyTeardown{r, cs, ns}
		if err := closedOffAfter(t, r, cs, ns, agentImage, leaky.KillSandbox); err == nil {
			t.Error("closedOffAfter passed a driver that deletes with zero grace and returns without waiting for the pods to go")
		} else {
			t.Logf("the early-returning driver is caught, as it must be: %v", err)
		}
	})
}

// closedOffAfter creates a sandbox, holds a connection from its agent to its proxy, runs teardown, and on
// return reports every way the run is still open: pods still listed for it, or the held connection still
// up after closedOffBound. It cleans up with the real runner. An environment problem (the connection
// never came up) fails t outright: that is no verdict either way.
func closedOffAfter(t *testing.T, r runner.Runner, cs kubernetes.Interface, ns, agentImage string, teardown func(context.Context, string) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		ProxyConfig:      conformance.LoadableProxyConfig(),
		Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_ = r.KillSandbox(stopCtx, sb.Ref)
	})
	runPods := metav1.ListOptions{LabelSelector: "wardyn.run-id=" + spec.RunID.String()}

	sess, err := r.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh", "-c", heldScript}})
	if err != nil {
		t.Fatalf("ExecStream (hold a connection to the proxy): %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	held, closed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(closed)
		sc := bufio.NewScanner(sess.Stdout)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "held=1" {
				close(held)
			}
		}
	}()
	select {
	case <-held:
	case <-closed:
		t.Fatal("the connection from the agent to its proxy did not stay up for 2 s — an environment problem, not a verdict: the check has nothing to find closed")
	case <-time.After(time.Minute):
		t.Fatal("the held-connection script never reported — an environment problem, not a verdict")
	}
	before, err := cs.CoreV1().Pods(ns).List(ctx, runPods)
	if err != nil || len(before.Items) == 0 {
		t.Fatalf("the run's pods before teardown (err %v, %d pods) — the positive control: with none listed, none listed afterwards proves nothing", err, len(before.Items))
	}

	if err := teardown(ctx, sb.Ref); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	var open []error
	after, err := cs.CoreV1().Pods(ns).List(ctx, runPods)
	if err != nil {
		t.Fatalf("list the run's pods after teardown: %v", err)
	}
	if n := len(after.Items); n > 0 {
		names := make([]string, n)
		for i, p := range after.Items {
			names[i] = fmt.Sprintf("%s (%s)", p.Name, p.Status.Phase)
		}
		open = append(open, fmt.Errorf("%d pod(s) still listed: %s", n, strings.Join(names, ", ")))
	}
	select {
	case <-closed:
	case <-time.After(closedOffBound):
		open = append(open, fmt.Errorf("the connection the agent held to its proxy was still up %s after teardown returned", closedOffBound))
	}
	return errors.Join(open...)
}

// apiServerProbeScript is a CODED probe (review round 2, H2): the original
// version asserted only "exit code != 0 means confined", which reads
// nc-missing (a broken image), KUBERNETES_SERVICE_HOST/PORT unset (a broken
// env), and a broken exec ALL as "confined" — none of them prove anything.
// Distinct exit codes separate "the probe could not run" from "the probe
// ran and found a breach":
//
//	90  nc is not on PATH                          — environment, not a verdict
//	91  KUBERNETES_SERVICE_HOST/PORT unset          — environment, not a verdict
//	92  the proxy sidecar (the ONE allowed peer) is unreachable — environment,
//	    not a verdict, but also a POSITIVE CONTROL: if the one peer the agent
//	    NetworkPolicy is supposed to allow can't be reached either, the probe
//	    proves nothing about confinement either way
//	93  the apiserver WAS reached                   — an actual L1 breach
//	0   the apiserver was NOT reached, everything else ran fine — confined
const apiServerProbeScript = `command -v nc >/dev/null 2>&1 || exit 90
[ -n "$KUBERNETES_SERVICE_HOST" ] && [ -n "$KUBERNETES_SERVICE_PORT" ] || exit 91
nc -z -w2 wardyn-proxy 3128 || exit 92
nc -z -w2 "$KUBERNETES_SERVICE_HOST" "$KUBERNETES_SERVICE_PORT" && exit 93
exit 0`

// testAgentCannotReachAPIServer is the k8s-local L1 replacement for the L0
// default-route probe: the agent pod's NetworkPolicy (internal/runner/k8s's
// CreateSandbox) allows egress ONLY to the proxy sidecar on
// runner.ProxyListenPort — no route to the Kubernetes API server. Every pod
// (regardless of ServiceAccount token automount — that is a separate
// mechanism) gets KUBERNETES_SERVICE_HOST/KUBERNETES_SERVICE_PORT env vars
// from the kubelet, so this needs no DNS (the agent's DNSPolicy is
// deliberately None). See apiServerProbeScript's doc for the exit-code
// contract this asserts against.
func testAgentCannotReachAPIServer(t *testing.T, r runner.Runner, agentImage string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}

	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		// The probe's own positive control dials the proxy sidecar, and the
		// REAL wardyn-proxy refuses a zero-value ProxyConfig; see
		// conformance.LoadableProxyConfig.
		ProxyConfig: conformance.LoadableProxyConfig(),
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer func() { _ = r.StopSandbox(context.Background(), sb.Ref) }()

	if _, err := r.Exec(ctx, sb.Ref, []string{"sh", "-c", apiServerProbeScript}); err != nil {
		t.Fatalf("Exec(apiserver probe): %v", err)
	}
	code, err := r.Wait(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	switch code {
	case 0:
		// confined: the proxy sidecar (the one allowed peer) was reachable,
		// the apiserver was not.
	case 90, 91, 92:
		t.Fatalf("apiserver probe could not run (exit %d) — an environment problem, not a confinement verdict either way: nc missing (90), KUBERNETES_SERVICE_HOST/PORT unset (91), or the proxy sidecar on 3128 unreachable (92 — the positive control: if the one allowed peer can't be reached, the probe proves nothing)", code)
	case 93:
		t.Error("agent pod reached the Kubernetes API server (KUBERNETES_SERVICE_HOST:PORT) directly — L1 confinement breach: the agent NetworkPolicy must allow ONLY the proxy sidecar")
	default:
		t.Fatalf("apiserver probe exited %d, a code the script never emits — investigate before trusting this result either way", code)
	}
}

// scratchWritableScript writes one byte to each scratch mount under its own exit
// code. It is the cheap answer to "an emptyDir arrives root-owned 0777 — is that
// actually writable by the agent's uid 1000?", and the codes are distinct per
// mount so a failure names WHICH path regressed instead of "something under the
// sandbox was read-only".
const scratchWritableScript = `echo scratch > /tmp/wardyn-scratch-probe || exit 91
echo scratch > /home/agent/work/wardyn-scratch-probe || exit 92
exit 0`

// testExecIsAcceptedWithADiskBudget is the assertion no fake clientset can make:
// that a REAL apiserver accepts the ephemeral container Exec builds for a run
// whose disk_mib is set.
//
// Why it is its own case. A disk budget makes CreateSandbox mount the two scratch
// emptyDirs on the main container, and Exec copies that container's VolumeMounts
// VERBATIM onto the ephemeral container. The apiserver refuses mount shapes THERE
// that it accepts on an ordinary container — a subPath above all ("Subpath mounts
// are not allowed for ephemeral containers"). A fake clientset records whatever
// the driver sends and calls it green, so the failure mode is a substrate where
// every autonomous run with a disk budget dies at dispatch while the unit suite
// stays green. The eviction case cannot stand in for this: it treats an Exec
// error as EXPECTED, because on a substrate that does enforce, the exec transport
// dies with the pod it is filling.
//
// It is a standing pin, and it deliberately says nothing about enforcement,
// only about admission and writability.
func testExecIsAcceptedWithADiskBudget(t *testing.T, r runner.Runner, agentImage string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("no ConfinementClasses declared; cannot create a sandbox")
	}
	spec := runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            agentImage,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		// Comfortably larger than the probe writes: this case is about ADMISSION,
		// and an eviction here would be a different (and confusing) verdict.
		Resources:   runner.Resources{DiskMiB: 512},
		ProxyConfig: conformance.LoadableProxyConfig(),
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox with DiskMiB %d: %v", spec.Resources.DiskMiB, err)
	}
	defer func() { _ = r.StopSandbox(context.Background(), sb.Ref) }()

	if _, err := r.Exec(ctx, sb.Ref, []string{"sh", "-c", scratchWritableScript}); err != nil {
		t.Fatalf("Exec on a sandbox with disk_mib set: %v — the apiserver REFUSED the ephemeral container. "+
			"Every autonomous k8s run with a disk budget fails at dispatch in this shape, and no fake-clientset test "+
			"can see it: check the mounts CreateSandbox puts on the main container (exec.go copies them verbatim, and "+
			"a subPath among them is rejected outright on an ephemeral container)", err)
	}
	code, err := r.Wait(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	switch code {
	case 0:
		// Admitted, and both scratch mounts were writable by the agent's uid.
	case 91, 92:
		t.Errorf("the agent could not write its own scratch (exit %d: 91=/tmp, 92=the workdir) — a disk budget "+
			"mounted over a path the agent cannot write is worse than no budget: the run cannot work at all", code)
	default:
		t.Fatalf("scratch-writability probe exited %d, a code the script never emits — investigate before trusting this result either way", code)
	}
}

// ephemeralStorageProbe is conformance.Options.EphemeralStorageProbe for this
// substrate: read the admitted agent pod and assert the ACCEPTED
// ephemeral-storage request is the small floor while the limit is the oversized
// number the case asked for. It builds its own clientset the same way the
// substrate's own config does (in-cluster first, then the default kubeconfig
// loading rules) rather than reaching into the driver, which exposes none.
func ephemeralStorageProbe(t *testing.T, ref string) {
	t.Helper()
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			t.Fatalf("kubeconfig for the ephemeral-storage read-back: %v", err)
		}
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("clientset for the ephemeral-storage read-back: %v", err)
	}
	ns := os.Getenv("WARDYN_K8S_NAMESPACE")
	if ns == "" {
		ns = "default" // k8s.Config.withDefaults' own fallback
	}
	pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pods get %s/%s: %v", ns, ref, err)
	}
	if pod.Spec.NodeName == "" {
		t.Fatalf("agent pod %s is not scheduled (phase %q) — an ephemeral-storage request copied from the limit is exactly what leaves it Pending", ref, pod.Status.Phase)
	}
	res := pod.Spec.Containers[0].Resources
	if got := res.Requests[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("256Mi")) != 0 {
		t.Errorf("accepted requests[ephemeral-storage] = %s, want 256Mi — the apiserver copies an unset request from the LIMIT, so anything else here means the floor was dropped", got.String())
	}
	if got := res.Limits[corev1.ResourceEphemeralStorage]; got.Cmp(resource.MustParse("200000Mi")) != 0 {
		t.Errorf("accepted limits[ephemeral-storage] = %s, want 200000Mi (the case's DiskMiB)", got.String())
	}
}
