// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// labelRecord stamps the agent pod with its run's recording choice at create, so every later exec
// follows the run's own choice and never this driver's Config.Record alone.
const labelRecord = "wardyn.record"

// recordsPod is whether an exec in pod is recorded: the pod's own choice within Config.Record. A pod
// created before the label existed follows Config.Record, as it did then.
func (d *Driver) recordsPod(pod *corev1.Pod) bool {
	v, ok := pod.Labels[labelRecord]
	if !ok {
		return d.cfg.Record
	}
	rec, _ := strconv.ParseBool(v)
	return d.cfg.Record && rec
}
