// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// RecoverOutput reads the exec container's log from its first byte: a finished
// container to its end without following, a running one followed; a pod that is
// gone is ErrSandboxGone, and a pod that never got the exec container has no
// output to read and no log read is made.
func TestRecoverOutput(t *testing.T) {
	withExec := func(st corev1.ContainerState) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: execContainerName}}}
			p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: execContainerName, State: st}}
		}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	terminated := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	neverStarts := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	for _, tc := range []struct {
		name       string
		shape      func(*corev1.Pod)
		deletePod  bool
		wantOut    string
		wantErr    error
		wantFollow *bool
	}{
		{name: "a finished container is read to its end, not followed", shape: withExec(terminated), wantOut: "all of it\n", wantFollow: new(false)},
		{name: "a running container is followed", shape: withExec(running), wantOut: "all of it\n", wantFollow: new(true)},
		{name: "a container that never starts has no log", shape: withExec(neverStarts)},
		{name: "a pod with no exec container has no output", shape: func(*corev1.Pod) {}},
		{name: "a pod that is gone", deletePod: true, wantErr: runner.ErrSandboxGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
			if tc.shape != nil {
				pod, err := cs.CoreV1().Pods(testNamespace).Get(t.Context(), ref, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				tc.shape(pod)
				if _, err := cs.CoreV1().Pods(testNamespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.deletePod {
				if err := cs.CoreV1().Pods(testNamespace).Delete(t.Context(), ref, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			var opts []*corev1.PodLogOptions
			cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				if a.GetSubresource() != "log" {
					return false, nil, nil
				}
				opts = append(opts, a.(clienttesting.GenericAction).GetValue().(*corev1.PodLogOptions))
				return true, &runtime.Unknown{Raw: []byte("all of it\n")}, nil
			})
			out := &lockedBuffer{}
			err := d.RecoverOutput(t.Context(), ref, out)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("RecoverOutput = %v, want %v", err, tc.wantErr)
			}
			if out.String() != tc.wantOut {
				t.Errorf("recovered %q, want %q", out.String(), tc.wantOut)
			}
			if tc.wantFollow == nil {
				if len(opts) != 0 {
					t.Fatalf("%d log reads, want none", len(opts))
				}
				return
			}
			if len(opts) != 1 || opts[0].Container != execContainerName || opts[0].Follow != *tc.wantFollow || opts[0].SinceTime != nil || opts[0].TailLines != nil {
				t.Fatalf("log reads %+v, want one of the exec container from its start with Follow=%v", opts, *tc.wantFollow)
			}
		})
	}
}

// A container that has not started yet is waited for, bounded by ctx.
func TestRecoverOutput_WaitIsBoundedByTheContext(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	pod, _ := cs.CoreV1().Pods(testNamespace).Get(t.Context(), ref, metav1.GetOptions{})
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: execContainerName}}}
	if _, err := cs.CoreV1().Pods(testNamespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := d.RecoverOutput(ctx, ref, &lockedBuffer{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RecoverOutput = %v, want the context's deadline", err)
	}
}
