// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// TestRunnerCreateSandbox_ShortRunRecordingLandsOnceTheProxyServes: a k8s runner's proxy binds
// its listener at once but refuses every agent request until its first renew succeeds, which can
// be a retry later. The agent pod must not exist before the proxy serves: a short run ends inside
// that window, and wardyn-rec's recording upload, its last act, is refused and lost.
//
// A kubelet stand-in marks the proxy pod Ready by the readiness probe CreateSandbox declares on it,
// against a proxy stand-in that answers as wardyn-proxy does (TestSelfLapse_ReadyPathFollowsTheGate
// pins those answers on the real proxy): 503 to everything while closed, 204 on proxy.ReadyPath
// once open, and a recording upload kept in a real recording store, standing in for the control
// plane behind it. The moment CreateSandbox returns, the agent runs: the real wardyn-rec, on the
// exact argv Exec puts on the ephemeral container, around a short command that prints a marker.
// The recording is then read back from the store and must hold that marker.
func TestRunnerCreateSandbox_ShortRunRecordingLandsOnceTheProxyServes(t *testing.T) {
	const marker = "wardyn-short-run-marker-7f3a"
	rec := buildWardynRec(t)
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var open atomic.Bool
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !open.Load():
			http.Error(w, "wardyn: this run's egress is closed", http.StatusServiceUnavailable)
		case r.Method == http.MethodGet && r.URL.Path == proxy.ReadyPath:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/wardyn/v1/recordings/"):
			if err := store.SaveCast(r.Context(), strings.TrimPrefix(r.URL.Path, "/wardyn/v1/recordings/"), r.Body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer px.Close()

	h := newRunnerHarness(t)
	h.d.cfg.Record = true
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	h.cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga := action.(clienttesting.GetAction)
		if !strings.HasPrefix(ga.GetName(), "wardyn-proxy-") {
			return false, nil, nil
		}
		obj, err := h.cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.PodIP, pod.Status.Phase = "10.244.0.7", corev1.PodRunning
		ready := probeSucceeds(px, pod.Spec.Containers[0].ReadinessProbe)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: proxyContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, Ready: ready,
		}}
		return true, pod, nil
	})
	installAgentRunningReactor(t, h.cs)

	// The first renew failed; its retry opens the proxy a little later.
	go func() {
		time.Sleep(300 * time.Millisecond)
		open.Store(true)
	}()
	spec := testSandboxSpec()
	spec.RunID = h.runID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sb, err := h.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := h.d.Exec(ctx, sb.Ref, []string{"sh", "-c", "echo " + marker}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	pod, err := h.cs.CoreV1().Pods(testNamespace).Get(ctx, sb.Ref, metav1.GetOptions{})
	if err != nil || len(pod.Spec.EphemeralContainers) != 1 {
		t.Fatalf("agent pod: %v", err)
	}
	runAgentLocally(t, rec, px, pod.Spec.EphemeralContainers[0].Command)

	rc, err := store.OpenCast(ctx, spec.RunID.String())
	if err != nil {
		t.Fatalf("no recording retrievable for the short run (%v): the agent started before its proxy served", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || !strings.Contains(string(got), marker) {
		t.Fatalf("recording %q (%v) does not hold the run's output %q", got, err, marker)
	}
}

// buildWardynRec builds the real recorder for this host.
func buildWardynRec(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "wardyn-rec")
	build := exec.Command("go", "build", "-o", bin, "github.com/cjohnstoniv/wardyn/cmd/wardyn-rec")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wardyn-rec: %v\n%s", err, out)
	}
	return bin
}

// runAgentLocally runs the ephemeral container's Command here: wardyn-rec is the built binary, its
// cast directory a temp dir, and wardyn-proxy is reached through HTTP_PROXY as the proxy stand-in.
// PATH holds only sh, so wardyn-rec records to a plain log (no asciinema), as the conformance image
// does.
func runAgentLocally(t *testing.T, rec string, px *httptest.Server, command []string) {
	t.Helper()
	if len(command) == 0 || command[0] != "wardyn-rec" {
		t.Fatalf("the agent's command is not the recorder: %q", command)
	}
	argv := append([]string{rec}, command[1:]...)
	for i := range argv[:len(argv)-1] {
		if argv[i] == "-cast-dir" {
			argv[i+1] = t.TempDir()
		}
	}
	path := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sh, filepath.Join(path, "sh")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + path, "HTTP_PROXY=" + px.URL, "http_proxy=" + px.URL}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent: %v\n%s", err, out)
	}
}
