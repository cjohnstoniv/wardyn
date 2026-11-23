// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// runService makes the run's Service, with no selector, and its EndpointSlice, empty, in server mode: the agent pins
// the Service's ClusterIP from its create, and the slice names the proxy pod once that pod has its IP
// (pointRunService). Runner mode makes neither and returns nil: its agent pins the proxy pod's IP, after the relay
// fence has passed it.
func (d *Driver) runService(ctx context.Context, ns string, runID uuid.UUID, labels map[string]string) (*corev1.Service, error) {
	if d.runnerMode() {
		return nil, nil
	}
	svc, err := d.createKeepService(ctx, ns, runID, labels)
	if err != nil {
		return nil, err
	}
	if err := d.writeKeepEndpoints(ctx, ns, runID, svc, ""); err != nil {
		return nil, err
	}
	return svc, nil
}

// pointRunService waits for the run's proxy pod to be Ready and points the run's EndpointSlice at it as soon as the pod
// has its IP. That is before Ready, so the cluster's Service routing has the proxy's own start to take up the endpoint,
// and the first exec, which waits for CreateSandbox, does not race it. The slice is written again only when the pod's
// address changes between reads. It returns the pod's primary IP at Ready.
func (d *Driver) pointRunService(ctx context.Context, clock *startClock, ns string, spec runner.SandboxSpec, svc *corev1.Service) (string, error) {
	// The empty slice took the Service's family, and writeKeepEndpoints refuses an address of the other, so on a
	// dual-stack cluster the slice names the proxy pod's address of the Service's family, not its primary one.
	family := addressFamily(svc.Spec.ClusterIP)
	var written string
	var sliceErr error
	ip, err := d.waitPodIPThen(ctx, clock, ns, proxyPodName(spec.RunID), spec.NotifyWaiting, func(pod *corev1.Pod) error {
		addr := podIPOfFamily(pod, family)
		if addr == written {
			return nil
		}
		if sliceErr = d.writeKeepEndpoints(ctx, ns, spec.RunID, svc, addr); sliceErr != nil {
			return sliceErr
		}
		written = addr
		return nil
	})
	if sliceErr != nil {
		return "", sliceErr
	}
	if err != nil {
		return "", fmt.Errorf("k8s: proxy pod never became ready: %w", err)
	}
	return ip, nil
}

// podIPOfFamily is the pod's address in family, from its PodIPs; on a single-stack cluster, or when the pod has none
// of that family yet, its primary PodIP, which writeKeepEndpoints then refuses by family (fail closed).
func podIPOfFamily(pod *corev1.Pod, family discoveryv1.AddressType) string {
	for _, a := range pod.Status.PodIPs {
		if addressFamily(a.IP) == family {
			return a.IP
		}
	}
	return pod.Status.PodIP
}

// proxyWaitSecsEnv is agent-run's bound on waiting for its proxy (wait_for_proxy, deploy/images/common/agent-run-lib.sh).
const proxyWaitSecsEnv = "WARDYN_PROXY_WAIT_SECS"

// proxyWaitEnv returns spec.Env with agent-run's proxy wait set to the whole start budget, for an interactive run in
// server mode: its agent pod starts beside its proxy, and agent-run --idle must hold until the proxy accepts a
// connection, which it can first do once the run's EndpointSlice names it. A value the run sets wins. It returns a
// copy, never the caller's map.
func (d *Driver) proxyWaitEnv(spec runner.SandboxSpec) map[string]string {
	if d.runnerMode() || !spec.Interactive {
		return spec.Env
	}
	if _, ok := spec.Env[proxyWaitSecsEnv]; ok {
		return spec.Env
	}
	env := maps.Clone(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	env[proxyWaitSecsEnv] = strconv.Itoa(int((d.cfg.StartTimeout+d.cfg.CapacityWait)/time.Second) + 1)
	return env
}
