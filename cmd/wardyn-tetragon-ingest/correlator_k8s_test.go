// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
)

const k8sRun = "5555aaaa-5555-5555-5555-55555555bbbb"

// k8sLine builds a canned Tetragon exec line for a pod.
func k8sLine(ns, pod, container string) []byte {
	return []byte(`{"node_name":"node-a","process_exec":{"process":{"binary":"/usr/bin/curl",` +
		`"docker":"c0ffee000000","pod":{"namespace":"` + ns + `","name":"` + pod + `",` +
		`"container":{"id":"c0ffee000000","name":"` + container + `"}}}}}`)
}

func TestPodCorrelatorMapsOnlyAgentPods(t *testing.T) {
	corr := newPodCorrelator([]string{"wardyn-runs", " other-runs "})
	mapper := &gatedMapper{inner: groundtruth.NewMapper(corr)}
	agent := "wardyn-agent-" + k8sRun
	cases := []struct {
		name string
		line []byte
		want bool
	}{
		{"agent container", k8sLine("wardyn-runs", agent, "agent"), true},
		{"exec container", k8sLine("wardyn-runs", agent, "wardyn-agent"), true},
		{"second configured namespace", k8sLine("other-runs", agent, "agent"), true},
		{"proxy pod", k8sLine("wardyn-runs", "wardyn-proxy-"+k8sRun, "wardyn-proxy"), false},
		{"proxy container in agent pod name", k8sLine("wardyn-runs", agent, "wardyn-proxy"), false},
		{"another namespace", k8sLine("kube-system", agent, "agent"), false},
		{"near-miss name suffix", k8sLine("wardyn-runs", agent+"-x", "agent"), false},
		{"near-miss name prefix", k8sLine("wardyn-runs", "x"+agent, "agent"), false},
		{"netpol-style name", k8sLine("wardyn-runs", "wardyn-agent-netpol-"+k8sRun, "agent"), false},
		{"upper-case uuid", k8sLine("wardyn-runs", "wardyn-agent-"+strings.ToUpper(k8sRun), "agent"), false},
		{"no dashes uuid", k8sLine("wardyn-runs", "wardyn-agent-"+strings.ReplaceAll(k8sRun, "-", ""), "agent"), false},
		{"empty uuid", k8sLine("wardyn-runs", "wardyn-agent-", "agent"), false},
		{"wrong container", k8sLine("wardyn-runs", agent, "sidecar"), false},
		{"empty container", k8sLine("wardyn-runs", agent, ""), false},
		{"no pod block", []byte(`{"process_exec":{"process":{"binary":"/bin/x","docker":"c0ffee000000"}}}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := mapper.MapLine(tc.line)
			if ok != tc.want {
				t.Fatalf("forwarded = %v, want %v", ok, tc.want)
			}
			if tc.want && (ev.RunID == nil || ev.RunID.String() != k8sRun) {
				t.Fatalf("run = %v, want %s", ev.RunID, k8sRun)
			}
		})
	}
	if got := mapper.droppedUnmappedCount(); got != 12 {
		t.Errorf("dropped_unmapped = %d, want 12 (every unmapped pod line counted)", got)
	}
}

// The pod correlator never resolves by container id, so a docker-style event
// cannot map in k8s mode.
func TestPodCorrelatorContainerIDNeverMaps(t *testing.T) {
	if _, ok := newPodCorrelator([]string{"wardyn-runs"}).RunForContainer("c0ffee000000"); ok {
		t.Fatal("RunForContainer mapped in k8s mode")
	}
}

// panicLister fails the test run if k8s mode touches docker at all.
type panicLister struct{}

func (panicLister) managedContainers(context.Context) ([]managedContainer, error) {
	panic("docker lister called in k8s mode")
}

func (panicLister) watchContainers(context.Context, func(managedContainer)) error {
	panic("docker watcher called in k8s mode")
}

func TestCorrelatorForK8sNeverBuildsDocker(t *testing.T) {
	corr, docker, err := correlatorFor("k8s", []string{"wardyn-runs"}, panicLister{}, panicLister{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if docker != nil {
		t.Fatal("k8s mode built a docker correlator (it would be refreshed and watched)")
	}
	mapper := &gatedMapper{inner: groundtruth.NewMapper(corr)}
	// A miss in k8s mode must not trigger an on-miss docker refresh either.
	mapper.MapLine(k8sLine("kube-system", "wardyn-agent-"+k8sRun, "agent"))
	mapper.MapLine([]byte(`{"process_exec":{"process":{"binary":"/bin/x","docker":"unknown"}}}`))
	if _, ok := mapper.MapLine(k8sLine("wardyn-runs", "wardyn-agent-"+k8sRun, "agent")); !ok {
		t.Fatal("agent pod did not map")
	}
}

func TestCorrelatorForDockerIsDefault(t *testing.T) {
	for _, source := range []string{"docker", ""} {
		lister := &fakeLister{}
		corr, docker, err := correlatorFor(source, nil, lister, &fakeWatcher{}, time.Second)
		if err != nil {
			t.Fatalf("source %q: %v", source, err)
		}
		if docker == nil || corr != groundtruth.Correlator(docker) {
			t.Fatalf("source %q did not build the docker correlator", source)
		}
	}
}

func TestCorrelatorForRefusesBadConfig(t *testing.T) {
	cases := []struct {
		source string
		ns     []string
		want   string
	}{
		{"kubernetes", []string{"x"}, "WARDYN_GROUNDTRUTH_SOURCE"},
		{"k8s", nil, "WARDYN_GROUNDTRUTH_K8S_NAMESPACES"},
		{"k8s", []string{" ", ""}, "WARDYN_GROUNDTRUTH_K8S_NAMESPACES"},
	}
	for _, tc := range cases {
		_, _, err := correlatorFor(tc.source, tc.ns, panicLister{}, panicLister{}, time.Second)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("correlatorFor(%q, %v) err = %v, want one naming %s", tc.source, tc.ns, err, tc.want)
		}
	}
}

func TestPodCorrelatorRunForPodReturnsParsedID(t *testing.T) {
	id, ok := newPodCorrelator([]string{"wardyn-runs"}).RunForPod("wardyn-runs", "wardyn-agent-"+k8sRun, "agent")
	if !ok || id != uuid.MustParse(k8sRun) {
		t.Fatalf("RunForPod = %v/%v", id, ok)
	}
}

// The heartbeat carries the sensor's node, on the beat and in its target.
func TestNodeHeartbeatCarriesNode(t *testing.T) {
	ev := heartbeatFor("node-a", 1, 2, 3, nil)
	if ev.Target != "node-a" || !strings.Contains(string(ev.Data), `"node":"node-a"`) {
		t.Fatalf("beat = target %q data %s", ev.Target, ev.Data)
	}
	if ev := heartbeatFor("", 1, 2, 3, nil); ev.Target != "" || strings.Contains(string(ev.Data), `"node"`) {
		t.Fatalf("compose beat changed: target %q data %s", ev.Target, ev.Data)
	}
}
