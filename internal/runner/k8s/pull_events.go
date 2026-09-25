// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// pullEventsEvery bounds how often one starting pod's Events are read. The pod
// poll runs every 200ms; a pull lasts minutes, and a once-a-second answer is
// all a progress step needs.
const pullEventsEvery = time.Second

// pullWatch is one wait's Events-reading state. The kubelet reports a pull as
// `ContainerCreating`, the same word it uses for everything else a container
// does before it starts; only the pod's Events say "Pulling" (#807).
type pullWatch struct {
	next time.Time // no Events read before this
	last string    // the last read's answer, held between reads
	off  bool      // a read failed: the rest of this wait uses the pod's own reason
}

// pullingDetail returns "image: Pulling: <ref>" — the Docker driver's exact
// words — while the kubelet is pulling container's image, and "" otherwise.
//
// FAIL CLOSED, both ways a claim could go wrong. It is only ever a label: any
// error reading Events (a 403 from an operator-written Role that predates the
// `events: list` grant, a slow apiserver) turns the read off for the rest of the
// wait and the pod's own reason stands; it never fails the create. And it never
// claims a pull it has not seen: see pullingFromEvents.
func (d *Driver) pullingDetail(ctx context.Context, pod *corev1.Pod, container string, w *pullWatch) string {
	if w.off || !containerCreating(pod, container) {
		w.last = ""
		return ""
	}
	now := time.Now()
	if now.Before(w.next) {
		return w.last
	}
	w.next = now.Add(pullEventsEvery)
	// The selector narrows the read server-side; pullingFromEvents re-checks every
	// field anyway, because a fake or a proxying apiserver may ignore it.
	sel := fields.Set{
		"involvedObject.kind": "Pod",
		"involvedObject.name": pod.Name,
		"involvedObject.uid":  string(pod.UID),
	}.AsSelector().String()
	evs, err := d.clientset.CoreV1().Events(d.cfg.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		w.off, w.last = true, ""
		slog.Debug("wardynd: k8s substrate: cannot read pod events; image pulls report as ContainerCreating",
			slog.String("pod", pod.Name), slog.Any("err", err))
		return ""
	}
	w.last = pullingFromEvents(pod, container, evs.Items)
	return w.last
}

// containerCreating reports whether container is Waiting in ContainerCreating —
// the only state a pull happens in. Every other reason (ImagePullBackOff,
// ErrImagePull, …) is already more specific than "Pulling" and must win.
func containerCreating(pod *corev1.Pod, container string) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container {
			return cs.State.Waiting != nil && cs.State.Waiting.Reason == "ContainerCreating"
		}
	}
	return false
}

// pullingFromEvents decides "pulling" from a pod's Events, and says so only when
// the kubelet has reported Pulling for THIS pod's container and has reported
// nothing else for it since. Any later kubelet event on that container (Pulled,
// Failed, BackOff, Created, Started…) means the pull is over, so the answer is "".
//
// Events are written by anything holding `events: create` in the namespace, so
// nothing an Event says reaches the reader: the image ref comes from the pod's
// own spec, and the Event only chooses between two strings Wardyn wrote. An
// Event for a same-named earlier pod (a different UID) is not this pod's.
//
// The list is assumed time-ordered — the apiserver's default List order for
// Events is creation/key order, and the kubelet writes Pulling before any
// later reason for the same container — so the last matching reason seen here
// wins. If that ever went the other way, the failure direction is safe: a
// pull reported out of order would either drop back to "" one tick early (the
// pod's own status still says ContainerCreating, so the step just re-lights
// on the next read) or, at worst, fail to light "Pulling" at all — never
// claim a pull that already finished.
func pullingFromEvents(pod *corev1.Pod, container string, evs []corev1.Event) string {
	image := ""
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			image = c.Image
		}
	}
	if image == "" || pod.UID == "" {
		return ""
	}
	fieldPath := "spec.containers{" + container + "}"
	pulling := false
	for _, e := range evs {
		o := e.InvolvedObject
		if o.Kind != "Pod" || o.Name != pod.Name || o.UID != pod.UID || o.FieldPath != fieldPath {
			continue
		}
		if e.Source.Component != "kubelet" && e.ReportingController != "kubelet" {
			continue
		}
		if e.Reason != "Pulling" {
			return ""
		}
		pulling = true
	}
	if !pulling {
		return ""
	}
	return "image: Pulling: " + image
}
