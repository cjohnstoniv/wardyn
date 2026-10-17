// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
)

// drainSpyBuffer is an ExecOutput that also implements runner.OutputDrainer.
type drainSpyBuffer struct {
	lockedBuffer
	dmu          sync.Mutex
	begun, ended int
	endErr       error
}

func (d *drainSpyBuffer) BeginDrain() { d.dmu.Lock(); d.begun++; d.dmu.Unlock() }
func (d *drainSpyBuffer) EndDrain(err error) {
	d.dmu.Lock()
	d.ended, d.endErr = d.ended+1, err
	d.dmu.Unlock()
}
func (d *drainSpyBuffer) counts() (begun, ended int, err error) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	return d.begun, d.ended, d.endErr
}

func (d *drainSpyBuffer) waitEnded(t *testing.T) error {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ended, err := d.counts(); ended == 1 {
			return err
		}
		if time.Now().After(deadline) {
			t.Fatal("the drain never ended")
		}
	}
}

// TestExec_ReportsTheLogFollowAsADrain: the follow is opened before Exec returns
// and ended once, cleanly, when the log reaches its end; a refused log read ends
// it with that error; and a container that will never start ends it cleanly with
// no log read, instead of leaving the owner to wait out its barrier.
func TestExec_ReportsTheLogFollowAsADrain(t *testing.T) {
	running := func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name: execContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
	}
	neverStarts := func(st *corev1.PodStatus) {
		st.EphemeralContainerStatuses = []corev1.ContainerStatus{{
			Name: execContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}
	}
	for _, tc := range []struct {
		name      string
		status    func(*corev1.PodStatus)
		logErr    error
		wantErr   bool
		wantReads int
	}{
		{"log read to its end", running, nil, false, 1},
		{"log read refused", running, errors.New("forbidden"), true, 1},
		{"container never starts", neverStarts, nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
			setPodStatus(t, cs, testNamespace, ref, tc.status)
			var mu sync.Mutex
			reads := 0
			cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				if a.GetSubresource() != "log" {
					return false, nil, nil
				}
				mu.Lock()
				reads++
				mu.Unlock()
				if tc.logErr != nil {
					return true, nil, tc.logErr
				}
				return true, &runtime.Unknown{Raw: []byte("ok\n")}, nil
			})
			out := &drainSpyBuffer{}
			d.execOutputs.Store(ref, out)
			if _, err := d.Exec(context.Background(), ref, []string{"/usr/local/bin/agent-run", "task"}); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if begun, _, _ := out.counts(); begun != 1 {
				t.Fatalf("drains begun when Exec returned = %d, want 1", begun)
			}
			if err := out.waitEnded(t); (err != nil) != tc.wantErr {
				t.Fatalf("drain ended with %v, want error=%v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if reads != tc.wantReads {
				t.Errorf("%d log reads, want %d", reads, tc.wantReads)
			}
		})
	}
}
