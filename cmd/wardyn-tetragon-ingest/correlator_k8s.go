// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// podCorrelator maps Kubernetes events to runs by the pod an event came from,
// read from the event itself. It needs no API-server access: a pod name is
// immutable, where labels are not, so labels are never the key.
//
// An event maps to a run only when its namespace is one of the configured run
// namespaces, its pod is named exactly wardyn-agent-<canonical uuid> (the name
// the k8s runner gives every agent pod), and its container is the agent's own
// ("agent", or "wardyn-agent", the one the exec path adds). Proxy pods and
// every other pod read as unmapped, so gatedMapper drops them as on Compose.
type podCorrelator struct {
	namespaces map[string]bool
}

const (
	agentPodNamePrefix = "wardyn-agent-" // mirrors internal/runner/k8s/naming.go (that package is build-tagged)
	agentContainerName = "agent"
	execContainerName  = "wardyn-agent"
)

func newPodCorrelator(namespaces []string) *podCorrelator {
	c := &podCorrelator{namespaces: map[string]bool{}}
	for _, ns := range namespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			c.namespaces[ns] = true
		}
	}
	return c
}

// RunForContainer never resolves: on Kubernetes the pod identity is the key,
// and a bare container id would carry no namespace to check.
func (*podCorrelator) RunForContainer(string) (uuid.UUID, bool) { return uuid.Nil, false }

// RunForPod implements groundtruth.PodCorrelator.
func (c *podCorrelator) RunForPod(namespace, name, container string) (uuid.UUID, bool) {
	if !c.namespaces[namespace] {
		return uuid.Nil, false
	}
	if container != agentContainerName && container != execContainerName {
		return uuid.Nil, false
	}
	suffix, ok := strings.CutPrefix(name, agentPodNamePrefix)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(suffix)
	// uuid.Parse also accepts other spellings; the runner only ever writes the canonical one.
	if err != nil || id.String() != suffix {
		return uuid.Nil, false
	}
	return id, true
}

// correlatorFor builds the correlator for WARDYN_GROUNDTRUTH_SOURCE. "docker"
// (or empty, the default) returns the docker correlator as both values, so the
// caller can refresh and watch it. "k8s" returns a pod correlator and a nil
// docker correlator: that mode never builds one, so it never shells out to
// docker.
func correlatorFor(source string, namespaces []string, lister dockerLister, watcher dockerWatcher, minRefreshGap time.Duration) (groundtruth.Correlator, *dockerCorrelator, error) {
	switch strings.TrimSpace(source) {
	case "", "docker":
		d := newDockerCorrelator(lister, watcher, minRefreshGap)
		return d, d, nil
	case "k8s":
		c := newPodCorrelator(namespaces)
		if len(c.namespaces) == 0 {
			return nil, nil, fmt.Errorf("WARDYN_GROUNDTRUTH_SOURCE=k8s needs WARDYN_GROUNDTRUTH_K8S_NAMESPACES (the namespaces Wardyn runs agent pods in): without one nothing can correlate")
		}
		return c, nil, nil
	default:
		return nil, nil, fmt.Errorf("WARDYN_GROUNDTRUTH_SOURCE must be docker or k8s, not %q", source)
	}
}

// heartbeatFor builds the sensor's liveness beat; node is the Kubernetes node
// name, empty on Compose.
func heartbeatFor(node string, dropped, observed, droppedUnmapped uint64, byKind map[string]uint64) types.AuditEvent {
	return groundtruth.HeartbeatEventForNode(node, dropped, observed, droppedUnmapped, byKind)
}
