// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// policyPeer is wardynd's own pods, a No network proxy's only peer.
var policyPeer = &ControlPlanePeer{Namespace: "wardyn", PodLabels: map[string]string{"app": "wardynd"}}

// addHold puts the import's hold beside whatever proxy policy the run has, with that policy's labels.
func addHold(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	canon := proxyPolicy(t, cs, proxyNetPolName(asRunID))
	if canon == nil {
		t.Fatal("the run has no proxy policy to copy the labels of")
	}
	hold := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: proxyHoldNetPolName(asRunID), Namespace: testNamespace, Labels: canon.Labels, UID: "hold-uid"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: canon.Spec.PodSelector,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	if err := cs.Tracker().Create(netpolsGVR, hold, testNamespace); err != nil {
		t.Fatal(err)
	}
}

// importedRun is a kept run in the state an import leaves it: no proxy pod, and the hold in place of the real policy.
func importedRun(t *testing.T) (*keepDriver, *fake.Clientset, string) {
	t.Helper()
	d, cs, _ := keepRig(t, Config{AgentSandboxKeep: true, ControlPlanePeer: policyPeer})
	sb, err := d.CreateSandbox(context.Background(), keptSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	addHold(t, cs)
	if err := cs.Tracker().Delete(netpolsGVR, testNamespace, proxyNetPolName(asRunID)); err != nil {
		t.Fatal(err)
	}
	if err := cs.Tracker().Delete(podsGVR, testNamespace, proxyPodName(asRunID)); err != nil {
		t.Fatal(err)
	}
	return &keepDriver{Driver: d}, cs, sb.Ref
}

func proxyPolicy(t *testing.T, cs *fake.Clientset, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	obj, err := cs.Tracker().Get(netpolsGVR, testNamespace, name)
	if err != nil {
		return nil
	}
	return obj.(*networkingv1.NetworkPolicy)
}

func TestReplaceProxy_ImportedRunWritesPolicyFromReviveConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  string
		pc   runner.ProxyConfig
	}{
		{"no network", `{"control_plane_url":"http://10.0.0.5:8080","no_network":true}`, runner.ProxyConfig{ControlPlaneURL: "http://10.0.0.5:8080", NoNetwork: true}},
		{"normal", `{"control_plane_url":"http://wardynd:8080"}`, runner.ProxyConfig{ControlPlaneURL: "http://wardynd:8080"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, cs, ref := importedRun(t)
			hold := proxyPolicy(t, cs, proxyHoldNetPolName(asRunID))
			order := namedWrites(cs, newFakeDynamic())
			if err := k.ReplaceProxy(context.Background(), ref, []byte(tc.cfg)); err != nil {
				t.Fatalf("ReplaceProxy: %v", err)
			}
			at := func(w string) int {
				return slices.IndexFunc(*order, func(s string) bool { return strings.HasPrefix(s, w) })
			}
			create, del := at("create networkpolicies "+proxyNetPolName(asRunID)), at("delete networkpolicies "+proxyHoldNetPolName(asRunID))
			if create != 0 || del != 1 || at("create secrets") < del || at("create pods") < del {
				t.Errorf("writes = %v, want the real policy created first, then the hold deleted, before any Secret or pod", *order)
			}
			canon := proxyPolicy(t, cs, proxyNetPolName(asRunID))
			if canon == nil {
				t.Fatal("no real proxy policy after the replace")
			}
			want, _, err := k.proxyEgressPlan(context.Background(), runner.SandboxSpec{RunID: asRunID, ProxyConfig: tc.pc})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(canon.Spec.Egress, want) {
				t.Errorf("egress = %+v\nwant %+v", canon.Spec.Egress, want)
			}
			if tc.pc.NoNetwork && (len(want) != 1 || len(want[0].To) != 1 || want[0].To[0].NamespaceSelector == nil) {
				t.Errorf("a No network proxy's egress = %+v, want the control-plane peer alone", want)
			}
			if !reflect.DeepEqual(canon.Labels, hold.Labels) {
				t.Errorf("labels = %v, want the hold's %v", canon.Labels, hold.Labels)
			}
			if len(canon.Spec.Ingress) != 1 || !reflect.DeepEqual(canon.Spec.Ingress[0].From[0].PodSelector.MatchLabels, wardynLabels(asRunID, componentAgent, nil)) {
				t.Errorf("ingress = %+v, want from the run's agent pods only", canon.Spec.Ingress)
			}
			if proxyPolicy(t, cs, proxyHoldNetPolName(asRunID)) != nil {
				t.Error("the hold is still there")
			}
		})
	}
}

func TestReplaceProxy_ImportedRunPolicyCreateFailsBeforeAnyWrite(t *testing.T) {
	k, cs, ref := importedRun(t)
	cs.PrependReactor("create", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("test: policy create refused")
	})
	order := namedWrites(cs, newFakeDynamic())
	err := k.ReplaceProxy(context.Background(), ref, replaceCfg)
	if err == nil || errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy = %v, want a plain error: nothing was changed", err)
	}
	if len(*order) != 1 || !strings.HasPrefix((*order)[0], "create networkpolicies") {
		t.Errorf("writes = %v, want only the refused policy create", *order)
	}
	if proxyPolicy(t, cs, proxyHoldNetPolName(asRunID)) == nil {
		t.Error("the hold is gone after a failed create")
	}
}

func TestReplaceProxy_HoldBesideCanonicalIsDeleted(t *testing.T) {
	k, cs, _, ref := keptRun(t)
	addHold(t, cs)
	cs.PrependReactor("delete", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("test: policy delete refused")
	})
	order := namedWrites(cs, newFakeDynamic())
	if err := k.ReplaceProxy(context.Background(), ref, replaceCfg); err != nil {
		t.Fatalf("ReplaceProxy with a hold whose delete fails = %v, want nil", err)
	}
	if slices.ContainsFunc(*order, func(s string) bool { return strings.HasPrefix(s, "create networkpolicies") }) {
		t.Errorf("writes = %v, want no policy create beside the real one", *order)
	}
	if !slices.Contains(*order, "delete networkpolicies "+proxyHoldNetPolName(asRunID)) {
		t.Errorf("writes = %v, want the hold's delete tried", *order)
	}
}

func TestReplaceProxy_HoldBesideProxyPodRefuses(t *testing.T) {
	k, cs, _, ref := keptRun(t)
	addHold(t, cs)
	if err := cs.Tracker().Delete(netpolsGVR, testNamespace, proxyNetPolName(asRunID)); err != nil {
		t.Fatal(err)
	}
	order := namedWrites(cs, newFakeDynamic())
	if err := k.ReplaceProxy(context.Background(), ref, replaceCfg); !errors.Is(err, runner.ErrProxyReplaceFailed) {
		t.Fatalf("ReplaceProxy with only the hold beside a proxy pod = %v, want ErrProxyReplaceFailed", err)
	}
	if len(*order) != 0 {
		t.Errorf("writes = %v, want none", *order)
	}
}
