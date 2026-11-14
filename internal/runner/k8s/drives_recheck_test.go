// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

var claimsGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

// onPodCreate runs act, once, inside the create of the first pod whose name starts with prefix, and counts the
// agent pods created; the create itself goes on to the tracker.
func onPodCreate(cs *fake.Clientset, prefix string, act func()) *int {
	agents, done := 0, false
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		if strings.HasPrefix(pod.Name, "wardyn-agent-") {
			agents++
		}
		if !done && strings.HasPrefix(pod.Name, prefix) {
			done = true
			act()
		}
		return false, nil, nil
	})
	return &agents
}

// TestCreateSandbox_RefusesADriveClaimReplacedWhileStarting: a run checks its drive's claim before its proxy
// starts and mounts it by name after. A claim reclaimed and made again under that name for another person in
// between is refused, before the agent pod exists when it happens during the proxy's start, and once the agent
// is running when it happens later; a claim that starts deleting is refused too. An unchanged claim passes.
func TestCreateSandbox_RefusesADriveClaimReplacedWhileStarting(t *testing.T) {
	replace := func(cs *fake.Clientset) func() {
		return func() {
			bob := existingDriveClaim(testDriveMount())
			bob.UID, bob.Labels[labelDriveSubject] = "uid-bob", "ffffffffffffffffffff"
			if err := cs.Tracker().Delete(claimsGVR, testNamespace, bob.Name); err != nil {
				t.Errorf("delete the judged claim: %v", err)
			}
			if err := cs.Tracker().Add(bob); err != nil {
				t.Errorf("add the replacement: %v", err)
			}
		}
	}
	terminate := func(cs *fake.Clientset) func() {
		return func() {
			obj, err := cs.Tracker().Get(claimsGVR, testNamespace, testDriveMount().ObjectName)
			if err != nil {
				t.Errorf("get the judged claim: %v", err)
				return
			}
			claim := obj.(*corev1.PersistentVolumeClaim).DeepCopy()
			now := metav1.Now()
			claim.DeletionTimestamp = &now
			if err := cs.Tracker().Update(claimsGVR, claim, testNamespace); err != nil {
				t.Errorf("mark the judged claim deleting: %v", err)
			}
		}
	}
	for _, tc := range []struct {
		name     string
		at       string
		act      func(*fake.Clientset) func()
		want     error
		noAgents bool
	}{
		{"replaced while the proxy starts", "wardyn-proxy-", replace, errDriveClaimVanished, true},
		{"replaced as the agent pod is made", "wardyn-agent-", replace, errDriveClaimVanished, false},
		{"deleting as the agent pod is made", "wardyn-agent-", terminate, errDriveClaimTerminating, false},
		{"unchanged", "", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			installProxyIPReactor(t, cs, "10.244.0.9")
			installAgentRunningReactor(t, cs)
			alice := existingDriveClaim(testDriveMount())
			alice.UID = k8stypes.UID("uid-alice")
			if err := cs.Tracker().Add(alice); err != nil {
				t.Fatal(err)
			}
			act := func() {}
			if tc.act != nil {
				act = tc.act(cs)
			}
			agents := onPodCreate(cs, tc.at, act)
			spec := testSandboxSpec()
			spec.Drives = []types.DriveMount{*testDriveMount()}
			_, err := d.CreateSandbox(context.Background(), spec)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("CreateSandbox = %v, want %v", err, tc.want)
			}
			if tc.noAgents && *agents != 0 {
				t.Errorf("an agent pod was created %d time(s) over a replaced claim; the refusal must come first", *agents)
			}
		})
	}
}
