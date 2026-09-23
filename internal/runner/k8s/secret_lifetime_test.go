// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// orderOf returns the resources of every recorded action with verb, in order.
func orderOf(cs *fake.Clientset, verb string, resources ...string) []string {
	want := make(map[string]bool, len(resources))
	for _, r := range resources {
		want[r] = true
	}
	var got []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && want[a.GetResource().Resource] {
			got = append(got, a.GetResource().Resource)
		}
	}
	return got
}

// TestTheSecretNeverOutlivesItsNetworkPolicies is the invariant that lets the
// orphan sweep find a run's Secret without holding `list` on secrets.
//
// 0.7.4 bought the both-pods-gone reclaim with a namespace-wide `secrets: list`
// — and a list returns every Secret's body, so with the default runsNamespace
// (the CONTROL-PLANE namespace) that is plaintext read of the DB DSN, the OIDC
// client secret and the ingress TLS key. 0.7.3 granted no such verb.
//
// The NetworkPolicies carry no credential, so listing THEM is free. Making them
// strictly outlive the Secret — created before it, deleted after it — turns the
// pair into a tombstone for the Secret: any Secret that still exists has both
// of its NetworkPolicies still there to be found by, and the sweep's own
// label-scoped deletecollection reclaims it. No window, no list, no verb.
func TestTheSecretNeverOutlivesItsNetworkPolicies(t *testing.T) {
	d, cs := newTestDriver(t, Config{})

	t.Run("CreateSandbox writes both NetworkPolicies BEFORE the Secret", func(t *testing.T) {
		cs.ClearActions()
		spec := createdSandbox(t, d, cs)
		got := orderOf(cs, "create", "secrets", "networkpolicies")
		want := []string{"networkpolicies", "networkpolicies", "secrets"}
		if len(got) != len(want) {
			t.Fatalf("create order = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("create order = %v, want %v — a Secret written first can outlive a crash that "+
					"never reaches the NetworkPolicy create, and nothing without `secrets: list` can then find it", got, want)
			}
		}
		_ = spec
	})

	t.Run("teardown deletes the Secret BEFORE the NetworkPolicies", func(t *testing.T) {
		spec := createdSandbox(t, d, cs)
		cs.ClearActions()
		if err := d.teardownByRunID(context.Background(), spec.RunID, ptrInt64(0)); err != nil {
			t.Fatalf("teardownByRunID: %v", err)
		}
		got := orderOf(cs, "delete-collection", "secrets", "networkpolicies")
		want := []string{"secrets", "networkpolicies"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("deletecollection order = %v, want %v — dropping the NetworkPolicies first leaves a "+
				"crash window in which the Secret survives with nothing left to find it by", got, want)
		}
	})
}

func ptrInt64(v int64) *int64 { return &v }

// TestSweepNeverListsSecrets is the RBAC bar itself, asserted against the code
// rather than the chart: the ServiceAccount must hold no Secret-BODY read
// capability beyond 0.7.3, and `list` is that capability (RBAC cannot scope a
// list by label). The sweep therefore keys on pods + networkpolicies only.
//
// The fixture is the very case `secrets: list` was added for — both pods gone,
// the Secret and both NetworkPolicies surviving — so this proves the reclaim
// still happens on the new keying, not merely that the call is absent.
func TestSweepNeverListsSecrets(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	spec := createdSandbox(t, d, cs)
	for _, name := range []string{agentPodName(spec.RunID), proxyPodName(spec.RunID)} {
		if err := cs.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("simulate a deleted node taking %q: %v", name, err)
		}
	}
	ageRunSecretsAndNetPols(t, cs, spec.RunID, time.Hour)

	// Scoped to the sweep alone: the assertions BELOW legitimately list Secrets
	// (that is a test's own bookkeeping, not a verb wardynd issues).
	cs.ClearActions()
	swept, err := d.SweepOrphanedSandboxes(context.Background(), time.Minute, alwaysOrphan)
	if got := orderOf(cs, "list", "secrets"); len(got) != 0 {
		t.Errorf("the orphan sweep issued %d Secrets().List call(s) — that needs a namespace-wide `secrets: list`, "+
			"which returns every Secret's body (DB DSN, OIDC client secret, ingress TLS key in the release namespace)", len(got))
	}
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1 — the run is reachable by its NetworkPolicy labels alone", swept)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}
