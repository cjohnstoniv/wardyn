// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// podFake is a Correlator that also implements PodCorrelator. It counts the
// container-id lookups so a test can see which path an event took.
type podFake struct {
	ns, name, container string
	run                 uuid.UUID
	containerCalls      int
}

func (f *podFake) RunForContainer(string) (uuid.UUID, bool) {
	f.containerCalls++
	return uuid.Nil, false
}

func (f *podFake) RunForPod(ns, name, container string) (uuid.UUID, bool) {
	if ns == f.ns && name == f.name && container == f.container {
		return f.run, true
	}
	return uuid.Nil, false
}

const podExecLine = `{"node_name":"node-a","process_exec":{"process":{"binary":"/usr/bin/curl","arguments":"x",` +
	`"docker":"abc123def456","pod":{"namespace":"wardyn-runs","name":"wardyn-agent-11111111-1111-1111-1111-111111111111",` +
	`"container":{"id":"abc123def456","name":"agent"}}}}}`

func TestMapperPodCorrelatorMapsPodEvent(t *testing.T) {
	corr := &podFake{ns: "wardyn-runs", name: "wardyn-agent-" + knownRun.String(), container: "agent", run: knownRun}
	ev, ok := NewMapper(corr).MapLine([]byte(podExecLine))
	if !ok {
		t.Fatal("pod exec line did not map")
	}
	if ev.RunID == nil || *ev.RunID != knownRun {
		t.Fatalf("run = %v, want %v", ev.RunID, knownRun)
	}
	d := decodeData(t, ev)
	if d.Correlation != CorrelationMapped {
		t.Errorf("correlation = %q, want mapped", d.Correlation)
	}
	if d.Node != "node-a" {
		t.Errorf("data.node = %q, want node-a (the event's node_name)", d.Node)
	}
	if d.Pod != "wardyn-runs/wardyn-agent-"+knownRun.String() {
		t.Errorf("data.pod = %q, want namespace/name", d.Pod)
	}
}

func TestMapperPodCorrelatorUnmappedPodEvent(t *testing.T) {
	corr := &podFake{ns: "other-ns", name: "x", container: "agent", run: knownRun}
	ev, ok := NewMapper(corr).MapLine([]byte(podExecLine))
	if !ok {
		t.Fatal("pod exec line did not map")
	}
	if ev.RunID != nil {
		t.Fatalf("run = %v, want nil (pod not known to the correlator)", ev.RunID)
	}
	if d := decodeData(t, ev); d.Correlation != CorrelationUnmapped {
		t.Errorf("correlation = %q, want unmapped", d.Correlation)
	}
}

// A non-pod event (no process.pod) still goes through the container-id path.
func TestMapperPodCorrelatorFallsBackToContainerID(t *testing.T) {
	corr := &podFake{}
	line := `{"process_exec":{"process":{"binary":"/bin/true","docker":"abc123def456"}}}`
	if _, ok := NewMapper(corr).MapLine([]byte(line)); !ok {
		t.Fatal("line did not map")
	}
	if corr.containerCalls != 1 {
		t.Fatalf("container lookups = %d, want 1", corr.containerCalls)
	}
}

// A plain Correlator (no RunForPod) ignores the pod block: the Compose path.
func TestMapperPlainCorrelatorIgnoresPodBlock(t *testing.T) {
	ev, ok := NewMapper(mappedCorrelator()).MapLine([]byte(podExecLine))
	if !ok {
		t.Fatal("line did not map")
	}
	if ev.RunID == nil || *ev.RunID != knownRun {
		t.Fatalf("run = %v, want %v via the container id", ev.RunID, knownRun)
	}
}

func TestMapperWithNodeFillsMissingNodeName(t *testing.T) {
	line := `{"process_exec":{"process":{"binary":"/bin/true","docker":"abc123def456"}}}`
	ev, _ := NewMapper(mappedCorrelator()).WithNode("node-b").MapLine([]byte(line))
	if d := decodeData(t, ev); d.Node != "node-b" {
		t.Errorf("data.node = %q, want node-b", d.Node)
	}
	ev, _ = NewMapper(mappedCorrelator()).WithNode("node-b").MapLine([]byte(podExecLine))
	if d := decodeData(t, ev); d.Node != "node-b" {
		t.Errorf("data.node = %q, want the sensor's node node-b", d.Node)
	}
}

// The sensor's own node wins over the export line's node_name: the heartbeat is
// keyed by the sensor's node, so events must carry the same value to join it.
func TestMapperNodeSensorWinsOverLine(t *testing.T) {
	ev, _ := NewMapper(mappedCorrelator()).WithNode("node-b").MapLine([]byte(podExecLine))
	if d := decodeData(t, ev); d.Node != "node-b" {
		t.Errorf("data.node = %q, want the sensor's node node-b over the line's node-a", d.Node)
	}
}

// With no sensor node the line's node_name is still used.
func TestMapperNodeFallsBackToLine(t *testing.T) {
	ev, _ := NewMapper(mappedCorrelator()).MapLine([]byte(podExecLine))
	if d := decodeData(t, ev); d.Node != "node-a" {
		t.Errorf("data.node = %q, want the line's node_name node-a", d.Node)
	}
}

func TestComposeEventCarriesNoNodeOrPod(t *testing.T) {
	line := `{"process_exec":{"process":{"binary":"/bin/true","docker":"abc123def456"}}}`
	ev, _ := NewMapper(mappedCorrelator()).MapLine([]byte(line))
	if bytes.Contains(ev.Data, []byte(`"node"`)) || bytes.Contains(ev.Data, []byte(`"pod"`)) {
		t.Errorf("compose event data has node/pod keys: %s", ev.Data)
	}
}

func TestHeartbeatEventForNode(t *testing.T) {
	ev := HeartbeatEventForNode("node-a", 1, 2, 3, nil)
	if ev.Target != "node-a" {
		t.Errorf("target = %q, want node-a", ev.Target)
	}
	var hb struct {
		Node          string `json:"node"`
		ObservedTotal uint64 `json:"observed_total"`
	}
	if err := json.Unmarshal(ev.Data, &hb); err != nil {
		t.Fatal(err)
	}
	if hb.Node != "node-a" || hb.ObservedTotal != 2 {
		t.Errorf("heartbeat data = %+v", hb)
	}
}

// With no node the beat is byte-identical to the one HeartbeatEventWithDropped
// has always built.
func TestHeartbeatEventForNodeEmptyIsUnchanged(t *testing.T) {
	a := HeartbeatEventWithDropped(3, 7, 11, map[string]uint64{ActionProcessExec: 7})
	b := HeartbeatEventForNode("", 3, 7, 11, map[string]uint64{ActionProcessExec: 7})
	if !bytes.Equal(a.Data, b.Data) || a.Target != "" || b.Target != "" {
		t.Errorf("empty node changed the beat: %s vs %s (target %q/%q)", a.Data, b.Data, a.Target, b.Target)
	}
	if bytes.Contains(a.Data, []byte(`"node"`)) {
		t.Errorf("compose beat carries a node key: %s", a.Data)
	}
}
