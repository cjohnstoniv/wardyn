// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// metricsMaxAge is how old a PodMetrics sample may be and still count. The
// metrics API refreshes about every 15 to 60 seconds; a sample older than this
// belongs to a pod whose kubelet stopped reporting, and no reading beats a stale one.
const metricsMaxAge = 3 * time.Minute

// BatchSample is true: one PodMetrics list reads every agent pod in the runs
// namespace (runner.ActivitySampler).
func (d *Driver) BatchSample() bool { return true }

// podMetricsList is the slice of metrics.k8s.io/v1beta1 PodMetricsList read here.
type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Timestamp  time.Time `json:"timestamp"`
		Containers []struct {
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// SampleCPU is runner.ActivitySampler: ONE PodMetrics list for the runs
// namespace, label-selected to agent pods, however many refs are asked for, and
// nothing run inside a sandbox. The proxy is another pod and is not selected.
// An agent pod's reading is the sum of its containers, the ephemeral one Exec
// adds included, since that is where the task runs.
//
// With no metrics-server, or a Role that may not list it, the signal is
// unavailable; a pod with no fresh sample (a young pod, a stale one) simply has
// no reading.
func (d *Driver) SampleCPU(ctx context.Context, refs []string) (map[string]float64, error) {
	raw, err := d.readPodMetrics(ctx, d.cfg.Namespace, labelManaged+"=true,"+labelComponent+"="+componentAgent)
	if err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			return nil, fmt.Errorf("%w: %v", runner.ErrActivityUnavailable, err)
		}
		return nil, fmt.Errorf("k8s: read pod metrics: %w", err)
	}
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("k8s: decode pod metrics: %w", err)
	}
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		want[r] = true
	}
	out := make(map[string]float64, len(refs))
	now := time.Now()
	for _, pod := range list.Items {
		name := pod.Metadata.Name
		if !want[name] || now.Sub(pod.Timestamp) > metricsMaxAge {
			continue
		}
		var nanos int64
		ok := len(pod.Containers) > 0
		for _, c := range pod.Containers {
			q, err := resource.ParseQuantity(c.Usage["cpu"])
			if err != nil {
				ok = false
				break
			}
			nanos += q.ScaledValue(resource.Nano)
		}
		if ok {
			out[name] = float64(nanos) / 1e9 * 100
		}
	}
	return out, nil
}

// readPodMetrics is the one metrics.k8s.io call, namespace-scoped, never
// cluster-wide. The client-go typed clientset has no metrics group, so it is a raw GET.
// A test replaces it through the Driver's metricsRead field.
func (d *Driver) readPodMetrics(ctx context.Context, ns, selector string) ([]byte, error) {
	if d.metricsRead != nil {
		return d.metricsRead(ctx, ns, selector)
	}
	return d.clientset.Discovery().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/"+ns+"/pods").
		Param("labelSelector", selector).DoRaw(ctx)
}
