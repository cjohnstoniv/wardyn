// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// lockedBuffer is a SandboxSpec.ExecOutput the follow goroutine and the test
// can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestExec_StreamsAgentLogIntoExecOutput: once the kubelet reports the agent
// exec container started, Exec's follower reads THAT container's log, with
// Follow, into SandboxSpec.ExecOutput; teardown forgets the writer.
func TestExec_StreamsAgentLogIntoExecOutput(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, ref, func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name: execContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
	})
	var mu sync.Mutex
	var asked *corev1.PodLogOptions
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		mu.Lock()
		asked = a.(clienttesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		mu.Unlock()
		return true, &runtime.Unknown{Raw: []byte("go test ./...\nok\n")}, nil
	})
	out := &lockedBuffer{}
	d.execOutputs.Store(ref, out) // what CreateSandbox stores for a spec carrying ExecOutput

	if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "go test ./..."}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); out.String() != "go test ./...\nok\n"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("exec output = %q, want the agent container's log", out.String())
		}
	}
	mu.Lock()
	if asked == nil || asked.Container != execContainerName || !asked.Follow {
		t.Errorf("log read options = %+v, want container %q with Follow", asked, execContainerName)
	}
	mu.Unlock()

	if err := d.KillSandbox(context.Background(), ref); err != nil {
		t.Fatalf("KillSandbox: %v", err)
	}
	if _, ok := d.execOutputs.Load(ref); ok {
		t.Fatal("teardown kept the sandbox's exec output writer")
	}
}

// TestExec_NoExecOutputReadsNoLog: a run that keeps no output tail costs no
// log read at all. Absence cannot be waited for, so a second pod's Exec that
// does keep a tail follows the same path with the same fake: its log read is
// the signal, awaited on a channel, and by then the first pod's follower (had
// it existed) has made the same read. Exactly one log read may have happened.
func TestExec_NoExecOutputReadsNoLog(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	running := func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name: execContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
	}
	quiet := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	tailed := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	setPodStatus(t, cs, testNamespace, quiet, running)
	setPodStatus(t, cs, testNamespace, tailed, running)

	var mu sync.Mutex
	logReads := 0
	firstRead := make(chan struct{})
	var once sync.Once
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		mu.Lock()
		logReads++
		mu.Unlock()
		once.Do(func() { close(firstRead) })
		return true, &runtime.Unknown{Raw: []byte("ok\n")}, nil
	})
	d.execOutputs.Store(tailed, &lockedBuffer{})

	if _, err := d.Exec(context.Background(), quiet, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec (no output): %v", err)
	}
	if _, err := d.Exec(context.Background(), tailed, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
		t.Fatalf("Exec (with output): %v", err)
	}
	select {
	case <-firstRead:
		// The goroutine just run was the newest; an older follower (had the quiet pod's Exec started
		// one) is queued behind it, so yield until it has had its turn before counting.
		for i := 0; i < 200; i++ {
			goruntime.Gosched()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pod that keeps an output tail never had its log read: the absence below would prove nothing")
	}
	mu.Lock()
	defer mu.Unlock()
	if logReads != 1 {
		t.Fatalf("log reads = %d, want 1: only the pod that keeps an output tail may be read", logReads)
	}
}

// TestCreateSandbox_KeepsExecOutputForExec: the writer a spec carries is
// what Exec later finds under the sandbox's ref.
func TestCreateSandbox_KeepsExecOutputForExec(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	out := &lockedBuffer{}
	spec.ExecOutput = out

	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if w, ok := d.execOutputs.Load(sb.Ref); !ok || w != out {
		t.Fatalf("execOutputs[%q] = %v, %v; want the spec's writer", sb.Ref, w, ok)
	}
}
