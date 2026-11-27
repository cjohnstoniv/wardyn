// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// keepDriver is the k8s substrate with keep on: the Driver plus the proxy replacement, keep and revive that only a run
// reaching its proxy through its own Service can take. Registered only then, so every other install answers as before.
type keepDriver struct{ *Driver }

// labelProxyCfg marks a replacement proxy config Secret with its stamp. Like every object of the run, the Secret carries
// the run's label, by which the run's teardown deletes one that no removed proxy pod mounted (removeProxyPod).
const labelProxyCfg = "wardyn.proxy-cfg"

// served is the substrate wardynd's registry hands out: with keep on the keepDriver that adds keep and revive, in server
// mode otherwise the serverDriver that revives a run kept after it was lost, and in runner mode the Driver.
func served(d *Driver) substrate.Substrate {
	if d.cfg.AgentSandbox && d.cfg.AgentSandboxKeep {
		return &keepDriver{Driver: d}
	}
	if !d.runnerMode() {
		return &serverDriver{Driver: d}
	}
	return d
}

// NewSubstrate is New, wrapped as wardynd's registry wraps it. Conformance builds its runner here so it drives the type
// wardynd serves; New alone returns the bare Driver.
func NewSubstrate(cfg Config) (substrate.Substrate, error) {
	d, err := New(cfg)
	if err != nil {
		return nil, err
	}
	return served(d), nil
}

// readRunService reads the run's namespace and its Service. A run with no Service answers runner.ErrReviveUnsupported.
func (d *Driver) readRunService(ctx context.Context, ref string) (uuid.UUID, string, *corev1.Service, error) {
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	ns, err := d.runNamespace(ctx, runID)
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	svc, err := d.clientset.CoreV1().Services(ns).Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return uuid.Nil, "", nil, runner.ErrReviveUnsupported
	}
	if err != nil {
		return uuid.Nil, "", nil, fmt.Errorf("k8s: read the run's Service: %w", err)
	}
	return runID, ns, svc, nil
}

// keptRunService is readRunService for a run kept at its end. Every server run has a Service, so kept-ness is read
// from the agent's Sandbox, as keptSandbox reads it. other is true for a run that is not kept at its end (its Sandbox
// is gone or not labelled kept): it answers as on an install with keep off (serverDriver), so a running one is never
// sent a new proxy, whose swap would garbage-collect its agent with the old proxy pod. A kept run whose Sandbox is
// being deleted, or with no Service, answers runner.ErrReviveUnsupported.
func (k *keepDriver) keptRunService(ctx context.Context, ref string) (runID uuid.UUID, ns string, svc *corev1.Service, other bool, err error) {
	runID, ns, svc, err = k.readRunService(ctx, ref)
	if err != nil {
		return uuid.Nil, "", nil, false, err
	}
	sb, err := k.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return uuid.Nil, "", nil, true, nil
	}
	if err != nil {
		return uuid.Nil, "", nil, false, fmt.Errorf("k8s: keep: read the agent's Sandbox: %w", err)
	}
	labels, _, _ := unstructured.NestedStringMap(sb.Object, "spec", "podTemplate", "metadata", "labels")
	if labels[labelKeep] != "true" {
		return uuid.Nil, "", nil, true, nil
	}
	if sb.GetDeletionTimestamp() != nil {
		return uuid.Nil, "", nil, false, runner.ErrReviveUnsupported
	}
	return runID, ns, svc, false, nil
}

// server is the keep backend's answer for a run not kept at its end: the one an install with keep off gives.
func (k *keepDriver) server() *serverDriver { return &serverDriver{Driver: k.Driver} }

// CanReplaceProxy answers nil for a kept run whose Service exists; any other run answers as with keep off.
func (k *keepDriver) CanReplaceProxy(ctx context.Context, ref string) error {
	_, _, _, other, err := k.keptRunService(ctx, ref)
	if other {
		return k.server().CanReplaceProxy(ctx, ref)
	}
	return err
}

// StopLostProxyAt keeps a lost run as the Driver does (runner.LostProxyStopper). A keep run's Sandbox is owned
// by the run's Service (keepAgent), so the proxy's delete releases nothing; any other run on a keep install is
// the plain agent-sandbox case. Declared here so the keep backend's answer is a choice, not an embedding.
func (k *keepDriver) StopLostProxyAt(ctx context.Context, ref string, gen int) error {
	return k.Driver.StopLostProxyAt(ctx, ref, gen)
}

// the cluster pulls the proxy image per its pull policy; nothing to stage
func (k *keepDriver) EnsureProxyImage(context.Context) error { return nil }

// replaceInputs reads what the new proxy pod needs from the config the API passes and from what Wardyn wrote when the
// run began. It changes nothing, so an error here leaves the old proxy running. It fills everything but SecretName, and
// also answers the proxy egress the config asks for and the run's proxy policies, which a moved run's replace writes from.
func (d *Driver) replaceInputs(ctx context.Context, ns string, runID uuid.UUID, cfgJSON []byte) (proxyPodInputs, []networkingv1.NetworkPolicyEgressRule, proxyPolicies, error) {
	var c struct {
		ControlPlaneURL string            `json:"control_plane_url"`
		AzureGates      []json.RawMessage `json:"azure_gates"`
		NoNetwork       bool              `json:"no_network"`
	}
	if err := json.Unmarshal(cfgJSON, &c); err != nil {
		return proxyPodInputs{}, nil, proxyPolicies{}, fmt.Errorf("k8s: keep: read the proxy config: %w", err)
	}
	spec := runner.SandboxSpec{RunID: runID, ProxyConfig: runner.ProxyConfig{ControlPlaneURL: c.ControlPlaneURL, NoNetwork: c.NoNetwork}}
	egress, pin, err := d.proxyEgressPlan(ctx, spec)
	if err != nil {
		return proxyPodInputs{}, nil, proxyPolicies{}, err
	}
	extra, pols, err := d.runExtraLabels(ctx, ns, runID)
	if err != nil {
		return proxyPodInputs{}, nil, proxyPolicies{}, err
	}
	deadline, err := d.agentDeadline(ctx, ns, runID)
	if err != nil {
		return proxyPodInputs{}, nil, proxyPolicies{}, err
	}
	return proxyPodInputs{
		RunID: runID, Namespace: ns, Deadline: deadline, ControlPlaneURL: c.ControlPlaneURL,
		Azure: len(c.AzureGates) > 0, Labels: extra, CPPin: pin,
	}, egress, pols, nil
}

// runExtraLabels is the run's own labels, read from its proxy NetworkPolicy, minus the three Wardyn reserves, with the
// run's proxy policies it found. A moved run not yet revived has only the import's hold policy, which carries the same
// labels.
//
// Wardyn wrote these labels when the run began; the run's spec is not kept here.
func (d *Driver) runExtraLabels(ctx context.Context, ns string, runID uuid.UUID) (map[string]string, proxyPolicies, error) {
	sel := labelRun + "=" + runID.String() + "," + labelComponent + "=" + componentProxy
	list, err := d.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, proxyPolicies{}, fmt.Errorf("k8s: keep: read the proxy NetworkPolicy: %w", err)
	}
	var pols proxyPolicies
	for i := range list.Items {
		switch list.Items[i].Name {
		case proxyNetPolName(runID):
			pols.canonical = &list.Items[i]
		case proxyHoldNetPolName(runID):
			pols.hold = &list.Items[i]
		}
	}
	from := cmp.Or(pols.canonical, pols.hold)
	if from == nil {
		return nil, pols, fmt.Errorf("k8s: keep: the run's proxy NetworkPolicy %s was not found", proxyNetPolName(runID))
	}
	extra := map[string]string{}
	for k, v := range from.Labels {
		if k != labelManaged && k != labelRun && k != labelComponent {
			extra[k] = v
		}
	}
	return extra, pols, nil
}

// agentDeadline is the active deadline of the run's agent pod template, or nil when it has none: the same backstop a
// revived agent pod takes, which counts from its own start.
func (d *Driver) agentDeadline(ctx context.Context, ns string, runID uuid.UUID) (*int64, error) {
	if !d.cfg.AgentSandbox {
		pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("k8s: read the agent pod: %w", err)
		}
		return pod.Spec.ActiveDeadlineSeconds, nil
	}
	sb, err := d.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: keep: read the agent's Sandbox: %w", err)
	}
	secs, found, err := unstructured.NestedInt64(sb.Object, "spec", "podTemplate", "spec", "activeDeadlineSeconds")
	if err != nil || !found {
		return nil, nil
	}
	return &secs, nil
}

// swapProxy empties the run's endpoint, removes old (the proxy pod the replace read; nil for none) with the replacement
// config Secret it mounted, starts the new one for policy generation gen and points the endpoint at it, while no newer
// proxy pod is there (pointKeepEndpointsAt). Every error from here on wraps runner.ErrProxyReplaceFailed: the old proxy
// may be gone.
func (d *Driver) swapProxy(ctx context.Context, ns string, runID uuid.UUID, svc *corev1.Service, in proxyPodInputs, old *corev1.Pod, gen int) error {
	fail := func(step string, err error) error {
		return fmt.Errorf("%w: %s: %v", runner.ErrProxyReplaceFailed, step, err)
	}
	// Until the new pod is made no pod mounts the config Secret this replace wrote, so a step that fails before then
	// deletes it.
	unmade := func(step string, err error) error {
		d.deleteUnmountedCfgSecret(ctx, ns, in.SecretName)
		return fail(step, err)
	}
	if err := d.emptyKeepEndpointsAt(ctx, ns, runID, svc, gen, true); err != nil {
		return unmade("empty the endpoint", err)
	}
	if err := d.removeProxyPod(ctx, ns, runID, old, false); err != nil {
		return unmade("remove the old proxy pod", err)
	}
	pod, err := d.buildProxyPod(in)
	if err != nil {
		return unmade("build the new proxy pod", err)
	}
	markReplacement(pod, gen)
	if err := d.createReplacementPod(ctx, ns, runID, pod, gen); err != nil {
		return unmade("create the new proxy pod", err)
	}
	ip, err := d.waitPodIP(ctx, d.newStartClock(), ns, proxyPodName(runID), nil)
	if err != nil {
		return fail("wait for the new proxy pod", err)
	}
	if err := d.pointKeepEndpointsAt(ctx, ns, runID, svc, gen, ip); err != nil {
		return fail("point the endpoint at the new proxy pod", err)
	}
	return nil
}

// ReplaceProxy is ReplaceProxyHashedAt for no generation.
func (k *keepDriver) ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error {
	_, err := k.ReplaceProxyHashedAt(ctx, ref, cfgJSON, 0)
	return err
}

// ReplaceProxyHashed is ReplaceProxyHashedAt for no generation.
func (k *keepDriver) ReplaceProxyHashed(ctx context.Context, ref string, cfgJSON []byte) (string, error) {
	return k.ReplaceProxyHashedAt(ctx, ref, cfgJSON, 0)
}

// ReplaceProxyHashedAt starts a new proxy for a run whose agent reaches it through the run's Service, acting for policy
// generation gen (runner.ProxyReviverAt), and answers the hash of the config it started it with. The run Secret is left
// as it is (the agent's env and files come from it); the new config goes in a Secret of its own. With gen > 0 the new
// proxy pod is labelled with gen, and a proxy pod labelled gen or newer refuses the replace with
// runner.ErrProxyGenerationNewer before anything is written. The proxy pod removed is the one read, never one that took
// the name since, and the replacement config Secret it mounted goes with it (removeProxyPod).
// The run's EndpointSlice is emptied only while no newer proxy pod is there (emptyKeepEndpointsAt), and a replace that
// fails before its new proxy pod is made deletes the config Secret it wrote. A moved run whose proxy policy is still the
// import's hold first gets its policy from cfgJSON, before any Secret or pod (settleProxyPolicy).
func (k *keepDriver) ReplaceProxyHashedAt(ctx context.Context, ref string, cfgJSON []byte, gen int) (string, error) {
	runID, ns, svc, other, err := k.keptRunService(ctx, ref)
	if other {
		return k.server().ReplaceProxyHashedAt(ctx, ref, cfgJSON, gen)
	}
	if err != nil {
		return "", err
	}
	return k.replaceProxyAt(ctx, ns, runID, svc, cfgJSON, gen, k.settleProxyPolicy)
}

// policySettler gives a moved run's proxy its policy before a replace writes anything (keepDriver.settleProxyPolicy).
type policySettler func(ctx context.Context, ns string, runID uuid.UUID, egress []networkingv1.NetworkPolicyEgressRule, pols proxyPolicies, old *corev1.Pod) error

// replaceProxyAt is ReplaceProxyHashedAt once the run's Service is read: the config Secret, the swap and the hash.
// settle is nil where no run is moved, and a run whose proxy policy is still a move's hold is then refused.
func (d *Driver) replaceProxyAt(ctx context.Context, ns string, runID uuid.UUID, svc *corev1.Service, cfgJSON []byte, gen int, settle policySettler) (string, error) {
	old, err := d.proxyNotNewer(ctx, ns, runID, gen, true)
	if err != nil {
		return "", err
	}
	in, egress, pols, err := d.replaceInputs(ctx, ns, runID, cfgJSON)
	if err != nil {
		return "", err
	}
	if settle != nil {
		if err := settle(ctx, ns, runID, egress, pols, old); err != nil {
			return "", err
		}
	} else if pols.hold != nil {
		return "", fmt.Errorf("%w: the run's proxy policy is still a move's hold", runner.ErrReviveUnsupported)
	}
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	labels := wardynLabels(runID, componentProxy, in.Labels)
	labels[labelProxyCfg] = stamp
	if gen > 0 {
		labels[labelProxyGeneration] = strconv.Itoa(gen)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(runID) + "-r" + stamp, Namespace: ns, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{proxyConfigSecretKey: cfgJSON},
	}
	if _, err := d.clientset.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("k8s: keep: create the new proxy config Secret: %w", err)
	}
	in.SecretName = sec.Name
	if err := d.swapProxy(ctx, ns, runID, svc, in, old, gen); err != nil {
		return "", err
	}
	return proxy.ConfigSHA256(cfgJSON), nil
}
