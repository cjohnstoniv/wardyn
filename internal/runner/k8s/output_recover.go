// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.OutputRecoverer = (*Driver)(nil)

// RecoverOutput implements runner.OutputRecoverer: it reads the agent exec
// container's log from its first byte into w. The kubelet keeps that log for as
// long as the pod lives, which is what makes the output recoverable here. A
// container that has terminated is read to its end (no follow); one still
// running is followed until it exits, which is how a process that adopts a live
// run resumes the capture. A pod that is gone is runner.ErrSandboxGone; a pod
// that never got the exec container has no output, which is an empty read.
// ctx bounds the whole read, the wait for a container that has not started yet
// included.
func (d *Driver) RecoverOutput(ctx context.Context, ref string, w io.Writer) error {
	pods := d.clientset.CoreV1().Pods(d.cfg.Namespace)
	for {
		pod, err := pods.Get(ctx, ref, metav1.GetOptions{})
		switch {
		case isNotFound(err):
			return runner.ErrSandboxGone
		case err != nil:
			return err
		}
		if !hasExecContainer(pod) || execContainerNeverStarts(pod) {
			return nil
		}
		if execContainerStarted(pod) {
			rc, err := pods.GetLogs(ref, &corev1.PodLogOptions{Container: execContainerName, Follow: !execContainerTerminated(pod)}).Stream(ctx)
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(w, rc)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(execWaitPollInterval):
		}
	}
}

// hasExecContainer reports whether the agent exec container was ever added to pod.
func hasExecContainer(pod *corev1.Pod) bool {
	for _, ec := range pod.Spec.EphemeralContainers {
		if ec.Name == execContainerName {
			return true
		}
	}
	return false
}

// execContainerTerminated reports whether the agent exec container has exited.
func execContainerTerminated(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if cs.Name == execContainerName {
			return cs.State.Terminated != nil
		}
	}
	return false
}
