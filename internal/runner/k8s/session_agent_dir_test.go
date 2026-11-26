// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// agentDirPod is an agent pod whose main container mounts the work folder's emptyDir (as a run with a disk size gets
// it, ephemeralScratchVolumes), with the agent exec'd (Exec copies that mount onto its container) and its exec
// container in state.
func agentDirPod(t *testing.T, d *Driver, cs *fake.Clientset, state corev1.ContainerState) string {
	t.Helper()
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	obj, err := cs.Tracker().Get(podsGVR, testNamespace, ref)
	if err != nil {
		t.Fatalf("tracker get: %v", err)
	}
	pod := obj.(*corev1.Pod).DeepCopy()
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: scratchWorkVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: scratchWorkVolumeName, MountPath: scratchWorkPath}}
	if err := cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatalf("tracker update: %v", err)
	}
	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: execContainerName, State: state}}
	})
	return ref
}

var (
	agentEnded   = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	agentRunning = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
)

// An exec confined to a folder on the volume the agent shares with the main container runs in the main container
// once the agent's container has ended: the kubelet runs nothing in an ended container.
func TestExecStream_AnEndedAgentsFolderOnASharedVolumeIsServedFromTheMainContainer(t *testing.T) {
	for _, dir := range []string{scratchWorkPath, scratchWorkPath + "/project", scratchWorkPath + "/project/"} {
		t.Run(dir, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			ref := agentDirPod(t, d, cs, agentEnded)
			fe := newFakeExecutor()
			d.execFactory = fe.execFactory
			sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"/usr/lib/openssh/sftp-server", "-e", "-d", dir}, AgentDir: dir})
			if err != nil {
				t.Fatalf("ExecStream: %v", err)
			}
			defer sess.Close()
			if fe.container != mainContainerName {
				t.Errorf("exec container = %q, want the main container %q", fe.container, mainContainerName)
			}
		})
	}
}

// Off every volume the two share, the main container shows its own filesystem, not the agent's: the exec is
// refused before any exec starts.
func TestExecStream_AnEndedAgentsFolderOffASharedVolumeIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, dir string }{
		{"the home folder above the volume", "/home/agent"},
		{"a sibling folder", "/home/agent/project"},
		{"a name that only starts like the volume", scratchWorkPath + "shop"},
		{"a relative path", "home/agent/work"},
		{"a deeper mount only the agent's container has", scratchWorkPath + "/inner/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			ref := agentDirPod(t, d, cs, agentEnded)
			if tc.dir == scratchWorkPath+"/inner/x" {
				obj, err := cs.Tracker().Get(podsGVR, testNamespace, ref)
				if err != nil {
					t.Fatalf("tracker get: %v", err)
				}
				pod := obj.(*corev1.Pod).DeepCopy()
				ec := &pod.Spec.EphemeralContainers[0]
				ec.VolumeMounts = append(ec.VolumeMounts, corev1.VolumeMount{Name: "agent-only", MountPath: scratchWorkPath + "/inner"})
				if err := cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
					t.Fatalf("tracker update: %v", err)
				}
			}
			fe := newFakeExecutor()
			d.execFactory = fe.execFactory
			_, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"cat", "x"}, AgentDir: tc.dir})
			if !errors.Is(err, runner.ErrAgentDirUnshared) {
				t.Fatalf("ExecStream = %v, want runner.ErrAgentDirUnshared", err)
			}
			if fe.container != "" {
				t.Errorf("an exec was built for container %q, want none", fe.container)
			}
		})
	}
}

// Controls: AgentDir changes nothing for an agent still running, for an exec that names no folder, or before the
// agent's container exists (the folder's first copy in runs in the main container already).
func TestExecStream_AgentDirLeavesEveryOtherExecWhereItWent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exec     bool
		state    corev1.ContainerState
		agentDir string
		want     string
	}{
		{"agent running", true, agentRunning, scratchWorkPath, execContainerName},
		{"agent ended, no folder named", true, agentEnded, "", execContainerName},
		{"no agent container yet", false, corev1.ContainerState{}, scratchWorkPath, mainContainerName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			var ref string
			if tc.exec {
				ref = agentDirPod(t, d, cs, tc.state)
			} else {
				ref = createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
			}
			fe := newFakeExecutor()
			d.execFactory = fe.execFactory
			sess, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"cat", "x"}, AgentDir: tc.agentDir})
			if err != nil {
				t.Fatalf("ExecStream: %v", err)
			}
			defer sess.Close()
			if fe.container != tc.want {
				t.Errorf("exec container = %q, want %q", fe.container, tc.want)
			}
		})
	}
}
