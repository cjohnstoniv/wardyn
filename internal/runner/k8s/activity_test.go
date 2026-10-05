// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// podMetric is one PodMetrics item: usage is each container's cpu quantity.
func podMetric(name string, age time.Duration, usage ...string) string {
	var cs []string
	for _, u := range usage {
		cs = append(cs, fmt.Sprintf(`{"name":"c","usage":{"cpu":%q,"memory":"10Mi"}}`, u))
	}
	return fmt.Sprintf(`{"metadata":{"name":%q},"timestamp":%q,"window":"15s","containers":[%s]}`,
		name, time.Now().Add(-age).UTC().Format(time.RFC3339), strings.Join(cs, ","))
}

func metricsList(items ...string) []byte {
	return []byte(`{"kind":"PodMetricsList","items":[` + strings.Join(items, ",") + `]}`)
}

// TestSampleCPU_OneNamespaceScopedListForTheWholeFleet: 50 sandboxes are read
// with one list, for the runs namespace, selected to agent pods (not proxies).
func TestSampleCPU_OneNamespaceScopedListForTheWholeFleet(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	var calls int
	var ns, selector string
	var items, refs []string
	for i := range 50 {
		name := fmt.Sprintf("wardyn-agent-%02d", i)
		refs = append(refs, name)
		items = append(items, podMetric(name, 20*time.Second, "250m"))
	}
	d.metricsRead = func(_ context.Context, n, sel string) ([]byte, error) {
		calls++
		ns, selector = n, sel
		return metricsList(items...), nil
	}
	got, err := d.SampleCPU(context.Background(), refs)
	if err != nil {
		t.Fatalf("SampleCPU: %v", err)
	}
	if calls != 1 || ns != testNamespace {
		t.Errorf("metrics calls = %d in %q, want one in %q", calls, ns, testNamespace)
	}
	if want := labelManaged + "=true," + labelComponent + "=" + componentAgent; selector != want {
		t.Errorf("label selector = %q, want %q", selector, want)
	}
	if len(got) != 50 || math.Abs(got[refs[0]]-25) > 0.001 {
		t.Errorf("readings = %d, first = %v; want 50 at 25 percent of a core", len(got), got[refs[0]])
	}
	if !d.BatchSample() {
		t.Error("BatchSample = false; one list reads the fleet")
	}
}

// TestSampleCPU_SumsEveryContainerOfThePod: the task runs in the ephemeral
// container Exec adds, beside the placeholder, and a sidecar's CPU is the pod's
// too, so the reading is the sum.
func TestSampleCPU_SumsEveryContainerOfThePod(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	d.metricsRead = func(context.Context, string, string) ([]byte, error) {
		return metricsList(podMetric("p", time.Second, "1m", "900m", "123456789n")), nil
	}
	got, err := d.SampleCPU(context.Background(), []string{"p"})
	if err != nil {
		t.Fatalf("SampleCPU: %v", err)
	}
	if want := (0.001 + 0.9 + 0.123456789) * 100; math.Abs(got["p"]-want) > 0.0001 {
		t.Errorf("reading = %v, want %v", got["p"], want)
	}
}

// TestSampleCPU_NoReadingIsAbsentNotZero: a pod the API has no fresh sample
// for, one asked for that it does not list, one with no containers, and a
// pod nobody asked about all leave the result without a quiet zero.
func TestSampleCPU_NoReadingIsAbsentNotZero(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	d.metricsRead = func(context.Context, string, string) ([]byte, error) {
		return metricsList(
			podMetric("fresh", time.Second, "500m"),
			podMetric("stale", 10*time.Minute, "500m"),
			podMetric("empty", time.Second),
			podMetric("garbled", time.Second, "lots"),
			podMetric("unasked", time.Second, "500m"),
		), nil
	}
	got, err := d.SampleCPU(context.Background(), []string{"fresh", "stale", "empty", "garbled", "young"})
	if err != nil {
		t.Fatalf("SampleCPU: %v", err)
	}
	if len(got) != 1 || got["fresh"] != 50 {
		t.Errorf("readings = %v, want only fresh at 50", got)
	}
}

// TestSampleCPU_SignalOffWithoutMetricsOrWithoutThePermission: no metrics API,
// a metrics-server that is down, and a Role that may not list are all "no
// signal"; any other failure is just a failed read.
func TestSampleCPU_SignalOffWithoutMetricsOrWithoutThePermission(t *testing.T) {
	gr := schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}
	for name, tc := range map[string]struct {
		err         error
		unavailable bool
	}{
		"no metrics API":      {apierrors.NewNotFound(gr, ""), true},
		"forbidden":           {apierrors.NewForbidden(gr, "", errors.New("no list")), true},
		"metrics-server down": {apierrors.NewServiceUnavailable("no endpoints"), true},
		"transient":           {errors.New("connection reset"), false},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := newTestDriver(t, Config{})
			d.metricsRead = func(context.Context, string, string) ([]byte, error) { return nil, tc.err }
			got, err := d.SampleCPU(context.Background(), []string{"p"})
			if err == nil || len(got) != 0 {
				t.Fatalf("readings = %v, err = %v; want an error and no readings", got, err)
			}
			if errors.Is(err, runner.ErrActivityUnavailable) != tc.unavailable {
				t.Errorf("unavailable = %v for %v, want %v", !tc.unavailable, err, tc.unavailable)
			}
		})
	}
}

// TestSampleCPU_TheRealReadIsOneNamespacedGET: with no test seam, the read is a
// single GET of the runs namespace's PodMetrics, label-selected, never
// cluster-wide, and a 403 from the API reads as no signal.
func TestSampleCPU_TheRealReadIsOneNamespacedGET(t *testing.T) {
	var paths, queries []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths, queries = append(paths, r.URL.Path), append(queries, r.URL.Query().Get("labelSelector"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))
			return
		}
		_, _ = w.Write(metricsList(podMetric("p", time.Second, "300m")))
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{clientset: cs, cfg: Config{Namespace: "runs-ns"}}

	got, err := d.SampleCPU(context.Background(), []string{"p"})
	if err != nil || got["p"] != 30 {
		t.Fatalf("readings = %v, err = %v; want p at 30", got, err)
	}
	if len(paths) != 1 || paths[0] != "/apis/metrics.k8s.io/v1beta1/namespaces/runs-ns/pods" ||
		queries[0] != "wardyn.managed=true,wardyn.component=agent" {
		t.Errorf("requests = %v %v, want one namespaced, label-selected list", paths, queries)
	}

	status = http.StatusForbidden
	if _, err := d.SampleCPU(context.Background(), []string{"p"}); !errors.Is(err, runner.ErrActivityUnavailable) {
		t.Errorf("err = %v, want unavailable for a Role that may not list", err)
	}
}
