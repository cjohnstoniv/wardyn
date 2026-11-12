// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// narrowExecutor is a Driver.execFactory double that plays one scripted exec: it records what the
// driver asked for and what it passed to the stream, then answers with its stdout, stderr and error.
type narrowExecutor struct {
	stdout, stderr string
	err            error

	podName, container string
	cmd                []string
	stdinArg, ttyArg   bool
	calls              int

	opts     remotecommand.StreamOptions
	gotStdin string
	deadline time.Time
	hasDL    bool
}

func (n *narrowExecutor) execFactory(podName, container string, cmd []string, stdin, tty bool) (remotecommand.Executor, error) {
	n.calls++
	n.podName, n.container, n.cmd, n.stdinArg, n.ttyArg = podName, container, cmd, stdin, tty
	return n, nil
}

func (n *narrowExecutor) Stream(opts remotecommand.StreamOptions) error {
	return n.StreamWithContext(context.Background(), opts)
}

func (n *narrowExecutor) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	n.opts = opts
	n.deadline, n.hasDL = ctx.Deadline()
	if opts.Stdin != nil {
		b, _ := io.ReadAll(opts.Stdin)
		n.gotStdin = string(b)
	}
	if opts.Stdout != nil {
		_, _ = io.WriteString(opts.Stdout, n.stdout)
	}
	if opts.Stderr != nil {
		_, _ = io.WriteString(opts.Stderr, n.stderr)
	}
	return n.err
}

// narrowProxyPod creates run runID's proxy pod, with or without the live deny env, running or not.
func narrowProxyPod(t *testing.T, cs *fake.Clientset, runID uuid.UUID, running, withEnv bool) {
	t.Helper()
	c := corev1.Container{Name: proxyContainerName}
	if withEnv {
		c.Env = []corev1.EnvVar{{Name: proxy.LiveDenyDirEnv, Value: proxyLiveDenyMountDir}}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: proxyPodName(runID), Namespace: testNamespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{c}},
	}
	if running {
		pod.Status.Phase = corev1.PodRunning
	}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create proxy pod: %v", err)
	}
}

func narrowJSON(t *testing.T, a proxy.NarrowAnswer) string {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// A server-mode proxy pod gets the env, a read-write mount and a default-medium emptyDir with a 1Mi
// size limit, and its staged config mount stays read-only. A runner's proxy pod gets none of them.
func TestProxyPod_LiveDenyVolumeAndEnv(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.9")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}
	c := containerByName(pod.Spec.Containers, proxyContainerName)
	if c == nil {
		t.Fatalf("proxy pod has no %q container", proxyContainerName)
	}
	envs := 0
	for _, e := range c.Env {
		if e.Name == proxy.LiveDenyDirEnv {
			envs++
			if e.Value != proxyLiveDenyMountDir {
				t.Errorf("%s = %q, want %q", proxy.LiveDenyDirEnv, e.Value, proxyLiveDenyMountDir)
			}
		}
	}
	if envs != 1 {
		t.Errorf("%s set %d times, want once: %+v", proxy.LiveDenyDirEnv, envs, c.Env)
	}
	mounts := 0
	for _, m := range c.VolumeMounts {
		switch m.Name {
		case proxyLiveDenyVolumeName:
			mounts++
			if m.MountPath != proxyLiveDenyMountDir || m.ReadOnly {
				t.Errorf("live deny mount = %+v, want a read-write mount at %s", m, proxyLiveDenyMountDir)
			}
		case proxyConfigStagedVolumeName:
			if !m.ReadOnly {
				t.Errorf("the staged config mount is no longer read-only: %+v", m)
			}
		}
	}
	if mounts != 1 {
		t.Errorf("live deny mount present %d times, want once: %+v", mounts, c.VolumeMounts)
	}
	vol, ok := findVolume(pod.Spec.Volumes, proxyLiveDenyVolumeName)
	if !ok || vol.EmptyDir == nil {
		t.Fatalf("no emptyDir volume %q: %+v", proxyLiveDenyVolumeName, pod.Spec.Volumes)
	}
	if vol.EmptyDir.Medium != corev1.StorageMediumDefault {
		t.Errorf("medium = %q, want the default: a memory volume would lose the list on a node reboot", vol.EmptyDir.Medium)
	}
	if vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.String() != "1Mi" {
		t.Errorf("size limit = %v, want 1Mi", vol.EmptyDir.SizeLimit)
	}

	// A runner's proxy pod never takes a narrowing, so it gets no socket whose failure could stop it.
	px := httptest.NewServer(http.NotFoundHandler())
	defer px.Close()
	h := newRunnerHarness(t)
	h.d.cfg.ProxyImage = "registry.example/wardyn-proxy:pinned"
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
	installAgentRunningReactor(t, h.cs)
	rspec := testSandboxSpec()
	rspec.RunID = h.runID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := h.d.CreateSandbox(ctx, rspec); err != nil {
		t.Fatalf("runner CreateSandbox: %v", err)
	}
	rpod, err := h.cs.CoreV1().Pods(testNamespace).Get(ctx, proxyPodName(rspec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get runner proxy pod: %v", err)
	}
	if _, has := findVolume(rpod.Spec.Volumes, proxyLiveDenyVolumeName); has {
		t.Errorf("a runner's proxy pod has the live deny volume")
	}
	for _, rc := range rpod.Spec.Containers {
		for _, e := range rc.Env {
			if e.Name == proxy.LiveDenyDirEnv {
				t.Errorf("a runner's proxy container has %s", proxy.LiveDenyDirEnv)
			}
		}
		for _, m := range rc.VolumeMounts {
			if m.Name == proxyLiveDenyVolumeName {
				t.Errorf("a runner's proxy container mounts the live deny volume")
			}
		}
	}
}

func TestProxyNarrowState_RunnerModeUnsupported(t *testing.T) {
	h := newRunnerHarness(t)
	narrowProxyPod(t, h.cs, h.runID, true, true)
	ex := &narrowExecutor{}
	h.d.execFactory = ex.execFactory
	ref := agentPodName(h.runID)
	if _, err := h.d.ProxyNarrowState(context.Background(), ref); !errors.Is(err, runner.ErrNarrowUnsupported) {
		t.Errorf("ProxyNarrowState in runner mode = %v, want ErrNarrowUnsupported", err)
	}
	if _, err := h.d.NarrowProxy(context.Background(), ref, []string{"a.example"}); !errors.Is(err, runner.ErrNarrowUnsupported) {
		t.Errorf("NarrowProxy in runner mode = %v, want ErrNarrowUnsupported", err)
	}
	if ex.calls != 0 {
		t.Errorf("runner mode ran %d execs, want none", ex.calls)
	}
}

func TestProxyNarrowState_PodWithoutEnvUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name         string
		running, env bool
	}{
		{"running, started before the env existed", true, false},
		{"has the env, not running", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			runID := uuid.New()
			narrowProxyPod(t, cs, runID, tc.running, tc.env)
			ex := &narrowExecutor{}
			d.execFactory = ex.execFactory
			ref := agentPodName(runID)
			if _, err := d.ProxyNarrowState(context.Background(), ref); !errors.Is(err, runner.ErrNarrowUnsupported) {
				t.Errorf("ProxyNarrowState = %v, want ErrNarrowUnsupported", err)
			}
			if _, err := d.NarrowProxy(context.Background(), ref, []string{"a.example"}); !errors.Is(err, runner.ErrNarrowUnsupported) {
				t.Errorf("NarrowProxy = %v, want ErrNarrowUnsupported", err)
			}
			if ex.calls != 0 {
				t.Errorf("an exec ran %d times, want none", ex.calls)
			}
		})
	}
}

func TestNarrowProxy_ExitCodeMapping(t *testing.T) {
	applied := proxy.NarrowAnswer{State: "applied", Changed: true, ConfigSHA256: "cfg", BaseSHA256: "base", LiveDenies: []string{"a.example"}}

	// The two-edit answer of 69,974 bytes is over the old 64 KiB cap and under the one bound.
	big := proxy.NarrowAnswer{State: "applied", Changed: true, ConfigSHA256: strings.Repeat("c", 64), BaseSHA256: strings.Repeat("b", 64)}
	for i := 0; i < 1000; i++ {
		big.LiveDenies = append(big.LiveDenies, fmt.Sprintf("h%04d.example", i))
	}
	pad := 69974 - len(narrowJSON(t, big))
	if pad < 0 {
		t.Fatalf("test answer is already %d bytes", len(narrowJSON(t, big)))
	}
	big.LiveDenies[len(big.LiveDenies)-1] += strings.Repeat("x", pad)
	bigLine := narrowJSON(t, big)
	if len(bigLine) != 69974 {
		t.Fatalf("two-edit answer is %d bytes, want 69974", len(bigLine))
	}

	exit := func(code int) error {
		return k8sexec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", code), Code: code}
	}
	overBound := strings.Repeat("x", proxy.MaxNarrowMessageBytes+1)

	for _, tc := range []struct {
		name           string
		stdout, stderr string
		err            error
		wantErr        error
		wantState      *runner.ProxyNarrowState
	}{
		{name: "exit 0 applied", stdout: narrowJSON(t, applied), wantState: &runner.ProxyNarrowState{ConfigSHA256: "cfg", BaseSHA256: "base", LiveDenies: []string{"a.example"}, Changed: true}},
		{name: "exit 0 current", stdout: narrowJSON(t, proxy.NarrowAnswer{State: "current", ConfigSHA256: "cfg", BaseSHA256: "base"}), wantState: &runner.ProxyNarrowState{ConfigSHA256: "cfg", BaseSHA256: "base"}},
		{name: "exit 0, the 69,974-byte two-edit answer", stdout: bigLine, wantState: &runner.ProxyNarrowState{ConfigSHA256: big.ConfigSHA256, BaseSHA256: big.BaseSHA256, LiveDenies: big.LiveDenies, Changed: true}},
		{name: "exit 0 refused", stdout: narrowJSON(t, proxy.NarrowAnswer{State: "refused", Error: "bad entry"}), wantErr: runner.ErrNarrowRefused},
		{name: "exit 2, an image without the flag", stderr: "flag provided but not defined: -narrow\nUsage of wardyn-proxy:\n", err: exit(2), wantErr: runner.ErrNarrowUnsupported},
		{name: "exit 2, a panic", stderr: "panic: runtime error: index out of range\n\ngoroutine 1 [running]:\n", err: exit(2), wantErr: runner.ErrNarrowUncertain},
		{name: "exit 3", err: exit(3), wantErr: runner.ErrNarrowNotListening},
		{name: "exit 4", err: exit(4), wantErr: runner.ErrNarrowUncertain},
		{name: "exec transport error", err: errors.New("websocket: close 1006"), wantErr: runner.ErrNarrowUncertain},
		{name: "stdout one byte over the bound", stdout: overBound, wantErr: runner.ErrNarrowUncertain},
		{name: "stdout that is not an answer", stdout: "not json\n", wantErr: runner.ErrNarrowUncertain},
		{name: "empty stdout on exit 0", wantErr: runner.ErrNarrowUncertain},
		{name: "an answer in a state nobody sends", stdout: narrowJSON(t, proxy.NarrowAnswer{State: "maybe"}), wantErr: runner.ErrNarrowUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			runID := uuid.New()
			narrowProxyPod(t, cs, runID, true, true)
			ex := &narrowExecutor{stdout: tc.stdout, stderr: tc.stderr, err: tc.err}
			d.execFactory = ex.execFactory
			got, err := d.NarrowProxy(context.Background(), agentPodName(runID), []string{"a.example"})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NarrowProxy = %+v, %v; want %v", got, err, tc.wantErr)
				}
				// Only an uncertain answer may wrap another sentinel's text: a definitive one must not be uncertain.
				if tc.wantErr != runner.ErrNarrowUncertain && errors.Is(err, runner.ErrNarrowUncertain) {
					t.Errorf("a definitive answer is also uncertain: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NarrowProxy: %v", err)
			}
			w := tc.wantState
			if got.ConfigSHA256 != w.ConfigSHA256 || got.BaseSHA256 != w.BaseSHA256 || got.Changed != w.Changed || strings.Join(got.LiveDenies, ",") != strings.Join(w.LiveDenies, ",") {
				t.Errorf("NarrowProxy state = %+v, want %+v", got, *w)
			}
		})
	}
}

// The exec reaches the proxy container of the run's proxy pod, with the narrowing binary, the list on
// stdin, no tty and a stderr stream of its own. The probe sends an empty list, and each is bounded.
func TestNarrowProxy_ExecTargetsTheProxyContainer(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	narrowProxyPod(t, cs, runID, true, true)
	ref := agentPodName(runID)
	ok := narrowJSON(t, proxy.NarrowAnswer{State: "applied", ConfigSHA256: "cfg"})

	ex := &narrowExecutor{stdout: ok}
	d.execFactory = ex.execFactory
	start := time.Now()
	if _, err := d.NarrowProxy(context.Background(), ref, []string{"a.example", "*.b.example"}); err != nil {
		t.Fatalf("NarrowProxy: %v", err)
	}
	if ex.podName != proxyPodName(runID) || ex.container != proxyContainerName {
		t.Errorf("exec target %s/%s, want %s/%s", ex.podName, ex.container, proxyPodName(runID), proxyContainerName)
	}
	if got := strings.Join(ex.cmd, " "); got != "/usr/local/bin/wardyn-proxy -narrow" {
		t.Errorf("argv = %q", got)
	}
	if !ex.stdinArg || ex.ttyArg {
		t.Errorf("newExecutor(stdin=%v, tty=%v), want stdin and no tty", ex.stdinArg, ex.ttyArg)
	}
	if ex.gotStdin != `{"deny":["a.example","*.b.example"]}` {
		t.Errorf("stdin = %q", ex.gotStdin)
	}
	if ex.opts.Tty || ex.opts.Stdout == nil || ex.opts.Stderr == nil || ex.opts.Stderr == ex.opts.Stdout {
		t.Errorf("stream options %+v: want no tty and separate stdout and stderr", ex.opts)
	}
	if !ex.hasDL || ex.deadline.Sub(start) > narrowExecTimeout+time.Second || ex.deadline.Sub(start) < narrowExecTimeout-time.Second {
		t.Errorf("NarrowProxy deadline in %v (set %v), want about %v", ex.deadline.Sub(start), ex.hasDL, narrowExecTimeout)
	}

	probe := &narrowExecutor{stdout: narrowJSON(t, proxy.NarrowAnswer{State: "current", ConfigSHA256: "cfg"})}
	d.execFactory = probe.execFactory
	start = time.Now()
	if _, err := d.ProxyNarrowState(context.Background(), ref); err != nil {
		t.Fatalf("ProxyNarrowState: %v", err)
	}
	if probe.gotStdin != `{"deny":[]}` {
		t.Errorf("probe stdin = %q, want an empty list", probe.gotStdin)
	}
	if !probe.hasDL || probe.deadline.Sub(start) > narrowProbeTimeout+time.Second || probe.deadline.Sub(start) < narrowProbeTimeout-time.Second {
		t.Errorf("ProxyNarrowState deadline in %v (set %v), want about %v", probe.deadline.Sub(start), probe.hasDL, narrowProbeTimeout)
	}
}

// eofExecutor plays the proxy's own client: it reads stdin to its end and answers only then, as
// wardyn-proxy -narrow does (it sends the request, half-closes, and waits for the answer). An exec whose
// stdin never ends gets no answer from it, only the context's end.
type eofExecutor struct {
	out string

	sawEOF bool
	gotIn  string
}

func (e *eofExecutor) execFactory(string, string, []string, bool, bool) (remotecommand.Executor, error) {
	return e, nil
}

func (e *eofExecutor) Stream(opts remotecommand.StreamOptions) error {
	return e.StreamWithContext(context.Background(), opts)
}

func (e *eofExecutor) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	if opts.Stdin == nil {
		return errors.New("no stdin was passed")
	}
	var in []byte
	done := make(chan struct{})
	go func() {
		in, _ = io.ReadAll(opts.Stdin)
		close(done)
	}()
	select {
	case <-done:
		e.sawEOF, e.gotIn = true, string(in)
		_, _ = io.WriteString(opts.Stdout, e.out)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestNarrowProxy_ClosesStdinAfterTheRequest: the exec's stdin ends after the request, so the proxy's
// client, which reads its stdin to the end before it dials, sends it and answers. The answer must come
// well inside the call's own bound (narrowProbeTimeout for a probe, narrowExecTimeout for a narrowing),
// which is itself above the client's own 10 s total.
func TestNarrowProxy_ClosesStdinAfterTheRequest(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	narrowProxyPod(t, cs, runID, true, true)
	ref := agentPodName(runID)
	const within = 2 * time.Second

	t.Run("a narrowing and a probe both reach EOF and get their answer", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			call  func(ctx context.Context) (runner.ProxyNarrowState, error)
			stdin string
		}{
			{"narrow", func(ctx context.Context) (runner.ProxyNarrowState, error) {
				return d.NarrowProxy(ctx, ref, []string{"a.example"})
			}, `{"deny":["a.example"]}`},
			{"probe", func(ctx context.Context) (runner.ProxyNarrowState, error) { return d.ProxyNarrowState(ctx, ref) }, `{"deny":[]}`},
		} {
			ex := &eofExecutor{out: narrowJSON(t, proxy.NarrowAnswer{State: "current", ConfigSHA256: "cfg", BaseSHA256: "base"})}
			d.execFactory = ex.execFactory
			// A parent bound far below the call's own, so an exec whose stdin never ends fails here at once.
			ctx, cancel := context.WithTimeout(context.Background(), within)
			start := time.Now()
			_, err := tc.call(ctx)
			cancel()
			if err != nil || !ex.sawEOF {
				t.Errorf("%s: err %v, stdin reached EOF %v: the request was never closed off, so the proxy's client would wait for it", tc.name, err, ex.sawEOF)
			}
			if ex.gotIn != tc.stdin {
				t.Errorf("%s: the exec read %q before EOF, want %q", tc.name, ex.gotIn, tc.stdin)
			}
			if took := time.Since(start); took >= within {
				t.Errorf("%s: the answer took %v, want under %v", tc.name, took, within)
			}
		}
	})

	t.Run("a fake exec that never closes stdin fails the check", func(t *testing.T) {
		pr, pw := io.Pipe()
		go func() { _, _ = pw.Write([]byte(`{"deny":[]}`)) }() // the request, and no close
		t.Cleanup(func() { _ = pw.Close() })
		ex := &eofExecutor{out: narrowJSON(t, proxy.NarrowAnswer{State: "current"})}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		var out strings.Builder
		err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: pr, Stdout: &out})
		if !errors.Is(err, context.DeadlineExceeded) || ex.sawEOF || out.Len() != 0 {
			t.Errorf("an exec whose stdin never ends: err %v, sawEOF %v, answer %q; want the deadline, no EOF and no answer", err, ex.sawEOF, out.String())
		}
	})
}
