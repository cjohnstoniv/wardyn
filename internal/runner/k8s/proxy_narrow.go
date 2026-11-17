// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	// proxyLiveDenyVolumeName is the proxy pod's own volume for the live deny list: node storage
	// that survives a container restart, so a node reboot keeps what the list holds. On a runner it
	// is memory-backed instead (see withLiveDenyDir).
	proxyLiveDenyVolumeName = "wardyn-proxy-live-denies"
	// proxyLiveDenyMountDir is where the proxy keeps that list and listens for narrowings.
	proxyLiveDenyMountDir = "/var/lib/wardyn-proxy-live"

	narrowProbeTimeout = 5 * time.Second
	narrowExecTimeout  = 15 * time.Second

	// narrowStderrBytes caps the stderr kept from a narrowing exec: enough to read a flag error.
	narrowStderrBytes = 4 << 10
	// narrowUnknownFlag is what Go's flag package prints on an image whose proxy has no -narrow.
	narrowUnknownFlag = "flag provided but not defined: -narrow"
)

// proxyLiveDenySizeLimit is a var because a quantity is addressed, not copied. The list is at most
// 270,337 bytes and holds no secret.
var proxyLiveDenySizeLimit = resource.MustParse("1Mi")

var _ runner.ProxyNarrower = (*Driver)(nil)

// withLiveDenyDir gives a proxy pod the volume, mount and env its live deny list needs. On a runner the
// volume is memory-backed: the proxy pod never restarts (RestartPolicy Never), so the list need not
// outlive it.
func withLiveDenyDir(pod *corev1.Pod, inMemory bool) {
	c := &pod.Spec.Containers[0]
	c.Env = append(c.Env, corev1.EnvVar{Name: proxy.LiveDenyDirEnv, Value: proxyLiveDenyMountDir})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: proxyLiveDenyVolumeName, MountPath: proxyLiveDenyMountDir})
	dir := &corev1.EmptyDirVolumeSource{SizeLimit: &proxyLiveDenySizeLimit}
	if inMemory {
		dir.Medium = corev1.StorageMediumMemory
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         proxyLiveDenyVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: dir},
	})
}

// ProxyNarrowState reads the live deny list of ref's proxy without changing it. On a runner it also
// proves the proxy still answers the base this runner recorded at create (BaseChecked).
func (d *Driver) ProxyNarrowState(ctx context.Context, ref string) (runner.ProxyNarrowState, error) {
	st, err := d.narrowExec(ctx, ref, nil, narrowProbeTimeout)
	if err != nil || !d.runnerMode() {
		return st, err
	}
	return d.baseChecked(ref, st, runner.ErrNarrowUnsupported)
}

// NarrowProxy adds denies to the live deny list of ref's proxy in place.
func (d *Driver) NarrowProxy(ctx context.Context, ref string, denies []string) (runner.ProxyNarrowState, error) {
	if d.runnerMode() {
		// The base is proved before anything is applied: a fault here changed nothing.
		if _, err := d.ProxyNarrowState(ctx, ref); err != nil {
			if errors.Is(err, runner.ErrNarrowUnsupported) || errors.Is(err, runner.ErrNarrowNotListening) {
				return runner.ProxyNarrowState{}, err
			}
			return runner.ProxyNarrowState{}, fmt.Errorf("%w: %v", runner.ErrNarrowNotSent, err) // %v: never also Uncertain
		}
	}
	st, err := d.narrowExec(ctx, ref, denies, narrowExecTimeout)
	if err != nil || !d.runnerMode() {
		return st, err
	}
	// After the apply, a fault may leave the list applied.
	return d.baseChecked(ref, st, runner.ErrNarrowUncertain)
}

// recordProxyBase saves the base of the config the run's proxy is handed, before the pod exists. A failed save
// stops the create, as openSink's does: a pod with no record could never be narrowed.
func (d *Driver) recordProxyBase(runID uuid.UUID, proxyJSON []byte) error {
	base, err := proxy.LiveDenyBaseSHA256(proxyJSON)
	if err != nil {
		return fmt.Errorf("k8s: proxy base: %w", err)
	}
	_, err = d.mutate(runID, func(ev *runEvidence) (bool, error) {
		ev.ProxyBaseSHA256 = base
		return true, nil
	})
	return err
}

// baseChecked compares st's base with the one recorded at create; fault is the answer when they differ or no
// record exists. It holds the run's lock only for the read, never across an exec.
func (d *Driver) baseChecked(ref string, st runner.ProxyNarrowState, fault error) (runner.ProxyNarrowState, error) {
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %v", fault, err)
	}
	ev, err := d.loadEvidence(runID)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %v", fault, err)
	}
	if ev.ProxyBaseSHA256 == "" {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: no base recorded for run %s", fault, runID)
	}
	if st.BaseSHA256 != ev.ProxyBaseSHA256 {
		slog.Warn("wardynd: k8s substrate: a proxy answers a base other than the one recorded at create; nothing was narrowed on it",
			slog.String("run", runID.String()), slog.String("answered", st.BaseSHA256), slog.String("recorded", ev.ProxyBaseSHA256))
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: the proxy's base is not the one recorded", fault)
	}
	st.BaseChecked = true
	return st, nil
}

// narrowExec runs wardyn-proxy -narrow in the proxy container with the request on stdin. An empty
// list is the probe. A proxy pod that cannot take a narrowing is refused before any exec.
func (d *Driver) narrowExec(ctx context.Context, ref string, denies []string, bound time.Duration) (runner.ProxyNarrowState, error) {
	ns, err := d.refNamespace(ctx, ref)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %w", runner.ErrNarrowNotSent, err)
	}
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %w", runner.ErrNarrowNotSent, err)
	}
	podName := proxyPodName(runID)
	pod, err := d.readPod(ctx, ns, podName)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: read proxy pod: %w", runner.ErrNarrowNotSent, err)
	}
	if pod.Status.Phase != corev1.PodRunning || !proxyTakesNarrowing(pod) {
		return runner.ProxyNarrowState{}, runner.ErrNarrowUnsupported
	}

	if denies == nil {
		denies = []string{}
	}
	req, err := json.Marshal(struct {
		Deny []string `json:"deny"`
	}{denies})
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %w", runner.ErrNarrowNotSent, err)
	}
	ex, err := d.newExecutor(ns, podName, proxyContainerName, []string{"/usr/local/bin/wardyn-proxy", "-narrow"}, true, false)
	if err != nil {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %v", runner.ErrNarrowNotSent, err)
	}
	ectx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	stdout := &cappedBuffer{max: proxy.MaxNarrowMessageBytes}
	stderr := &cappedBuffer{max: narrowStderrBytes}
	execErr := ex.StreamWithContext(ectx, remotecommand.StreamOptions{
		Stdin:  bytes.NewReader(req),
		Stdout: stdout,
		Stderr: stderr,
	})
	return narrowResult(execErr, stdout, stderr)
}

// proxyTakesNarrowing reports whether the proxy container was created with the live deny directory:
// a pod an older server started has none, and no exec can give it one.
func proxyTakesNarrowing(pod *corev1.Pod) bool {
	c, ok := findContainer(pod.Spec.Containers, proxyContainerName)
	if !ok {
		return false
	}
	for _, e := range c.Env {
		if e.Name == proxy.LiveDenyDirEnv {
			return true
		}
	}
	return false
}

// narrowResult maps an exec's outcome to the runner's answers. Only an exit that proves nothing
// was delivered is definitive; any other failure may have reached the proxy and is uncertain.
func narrowResult(execErr error, stdout, stderr *cappedBuffer) (runner.ProxyNarrowState, error) {
	uncertain := func(cause error) (runner.ProxyNarrowState, error) {
		return runner.ProxyNarrowState{}, fmt.Errorf("k8s: narrow proxy: %w: %v", runner.ErrNarrowUncertain, cause)
	}
	if execErr != nil {
		var ce k8sexec.CodeExitError
		if errors.As(execErr, &ce) {
			switch {
			case ce.Code == 2 && strings.Contains(stderr.buf.String(), narrowUnknownFlag):
				return runner.ProxyNarrowState{}, runner.ErrNarrowUnsupported
			case ce.Code == 3:
				return runner.ProxyNarrowState{}, runner.ErrNarrowNotListening
			}
		}
		return uncertain(execErr)
	}
	if stdout.over {
		return uncertain(fmt.Errorf("answer is over %d bytes", stdout.max))
	}
	var ans proxy.NarrowAnswer
	if err := json.Unmarshal(stdout.buf.Bytes(), &ans); err != nil {
		return uncertain(fmt.Errorf("answer does not decode: %w", err))
	}
	switch ans.State {
	case "refused":
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %s", runner.ErrNarrowRefused, ans.Error)
	case "current", "applied":
		return runner.ProxyNarrowState{
			ConfigSHA256: ans.ConfigSHA256,
			BaseSHA256:   ans.BaseSHA256,
			LiveDenies:   ans.LiveDenies,
			Changed:      ans.Changed,
		}, nil
	}
	return uncertain(fmt.Errorf("answer state %q", ans.State))
}

// cappedBuffer keeps at most max bytes and notes that more arrived, so a stream is drained to its
// end without growing past the bound.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - c.buf.Len(); n > room {
		c.over = true
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}
