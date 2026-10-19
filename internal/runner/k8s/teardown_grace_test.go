// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestServerModeTeardownNeverForceDeletesPods: a zero-grace delete removes a pod
// from the API before the kubelet has stopped it, so the "pods gone" list that
// holds the NetworkPolicies back would prove nothing. Every server-mode pod
// delete (kill, create rollback, orphan sweep) asks for a grace the kubelet must
// confirm, and the NetworkPolicies drop only after that list.
func TestServerModeTeardownNeverForceDeletesPods(t *testing.T) {
	for name, tear := range map[string]func(t *testing.T, d *Driver, cs *fake.Clientset){
		"kill": func(t *testing.T, d *Driver, cs *fake.Clientset) {
			spec := createdSandbox(t, d, cs)
			cs.ClearActions()
			if err := d.KillSandbox(context.Background(), agentPodName(spec.RunID)); err != nil {
				t.Fatalf("KillSandbox: %v", err)
			}
		},
		"create rollback": func(t *testing.T, d *Driver, cs *fake.Clientset) {
			installProxyIPReactor(t, cs, "10.244.0.7")
			cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if ga, ok := action.(clienttesting.GetAction); ok && strings.HasPrefix(ga.GetName(), "wardyn-agent-") {
					return true, nil, errors.New("simulated: agent pod never starts")
				}
				return false, nil, nil
			})
			cs.ClearActions()
			if _, err := d.CreateSandbox(context.Background(), testSandboxSpec()); err == nil {
				t.Fatal("CreateSandbox: want an error, got nil")
			}
		},
		"orphan sweep": func(t *testing.T, d *Driver, cs *fake.Clientset) {
			spec := createdSandbox(t, d, cs)
			agePodsOfRun(t, cs, spec.RunID, time.Hour)
			cs.ClearActions()
			if n, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan); err != nil || n != 1 {
				t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 1, nil", n, err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond})
			tear(t, d, cs)
			podDelete, podList, netpolDelete := -1, -1, -1
			for i, a := range cs.Actions() {
				res := a.GetResource().Resource
				switch {
				case a.GetVerb() == "delete-collection" && res == "pods":
					if podDelete == -1 {
						podDelete = i
					}
					opts := a.(interface{ GetDeleteOptions() metav1.DeleteOptions }).GetDeleteOptions()
					if g := opts.GracePeriodSeconds; g != nil && *g < 1 {
						t.Errorf("pod delete-collection with grace %d: a force delete leaves no proof the pod stopped", *g)
					}
				case a.GetVerb() == "list" && res == "pods" && podDelete != -1 && podList == -1:
					podList = i
				case a.GetVerb() == "delete-collection" && res == "networkpolicies" && netpolDelete == -1:
					netpolDelete = i
				}
			}
			if podDelete == -1 || !(podDelete < podList && podList < netpolDelete) {
				t.Errorf("want pod delete-collection < pod list < netpol delete-collection, got %d, %d, %d", podDelete, podList, netpolDelete)
			}
		})
	}
}
