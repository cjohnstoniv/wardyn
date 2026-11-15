// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
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

// served is the substrate wardynd's registry hands out: the Driver, or with keep on the keepDriver that adds keep and revive.
func served(d *Driver) substrate.Substrate {
	if d.cfg.AgentSandbox && d.cfg.AgentSandboxKeep {
		return &keepDriver{Driver: d}
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

// runService reads the run's namespace and its Service. A run with no Service is not one that can be kept, and
// answers runner.ErrReviveUnsupported.
func (k *keepDriver) runService(ctx context.Context, ref string) (uuid.UUID, string, *corev1.Service, error) {
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	ns, err := k.runNamespace(ctx, runID)
	if err != nil {
		return uuid.Nil, "", nil, err
	}
	svc, err := k.clientset.CoreV1().Services(ns).Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return uuid.Nil, "", nil, runner.ErrReviveUnsupported
	}
	if err != nil {
		return uuid.Nil, "", nil, fmt.Errorf("k8s: keep: read the run's Service: %w", err)
	}
	return runID, ns, svc, nil
}

// CanReplaceProxy answers nil only for a run whose Service exists.
func (k *keepDriver) CanReplaceProxy(ctx context.Context, ref string) error {
	_, _, _, err := k.runService(ctx, ref)
	return err
}

// the cluster pulls the proxy image per its pull policy; nothing to stage
func (k *keepDriver) EnsureProxyImage(context.Context) error { return nil }

// replaceInputs reads what the new proxy pod needs from the config the API passes and from what Wardyn wrote when the
// run began. It changes nothing, so an error here leaves the old proxy running. It fills everything but SecretName.
func (d *Driver) replaceInputs(ctx context.Context, ns string, runID uuid.UUID, cfgJSON []byte) (proxyPodInputs, error) {
	var c struct {
		ControlPlaneURL string            `json:"control_plane_url"`
		AzureGates      []json.RawMessage `json:"azure_gates"`
		NoNetwork       bool              `json:"no_network"`
	}
	if err := json.Unmarshal(cfgJSON, &c); err != nil {
		return proxyPodInputs{}, fmt.Errorf("k8s: keep: read the proxy config: %w", err)
	}
	spec := runner.SandboxSpec{RunID: runID, ProxyConfig: runner.ProxyConfig{ControlPlaneURL: c.ControlPlaneURL, NoNetwork: c.NoNetwork}}
	_, pin, err := d.proxyEgressPlan(ctx, spec)
	if err != nil {
		return proxyPodInputs{}, err
	}
	extra, err := d.runExtraLabels(ctx, ns, runID)
	if err != nil {
		return proxyPodInputs{}, err
	}
	deadline, err := d.agentDeadline(ctx, ns, runID)
	if err != nil {
		return proxyPodInputs{}, err
	}
	return proxyPodInputs{
		RunID: runID, Namespace: ns, Deadline: deadline, ControlPlaneURL: c.ControlPlaneURL,
		Azure: len(c.AzureGates) > 0, Labels: extra, CPPin: pin,
	}, nil
}

// runExtraLabels is the run's own labels, read from its proxy NetworkPolicy, minus the three Wardyn reserves.
//
// Wardyn wrote these labels when the run began; the run's spec is not kept here.
func (d *Driver) runExtraLabels(ctx context.Context, ns string, runID uuid.UUID) (map[string]string, error) {
	sel := labelRun + "=" + runID.String() + "," + labelComponent + "=" + componentProxy
	pols, err := d.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("k8s: keep: read the proxy NetworkPolicy: %w", err)
	}
	for _, p := range pols.Items {
		if p.Name != proxyNetPolName(runID) {
			continue
		}
		extra := map[string]string{}
		for k, v := range p.Labels {
			if k != labelManaged && k != labelRun && k != labelComponent {
				extra[k] = v
			}
		}
		return extra, nil
	}
	return nil, fmt.Errorf("k8s: keep: the run's proxy NetworkPolicy %s was not found", proxyNetPolName(runID))
}

// agentDeadline is the active deadline of the run's agent pod template, or nil when it has none: the same backstop a
// revived agent pod takes, which counts from its own start.
func (d *Driver) agentDeadline(ctx context.Context, ns string, runID uuid.UUID) (*int64, error) {
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
// config Secret it mounted, starts the new one for policy generation gen and points the endpoint at it. Every error from
// here on wraps runner.ErrProxyReplaceFailed: the old proxy may be gone.
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
	if err := d.removeProxyPod(ctx, ns, runID, old); err != nil {
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
	if err := d.writeKeepEndpoints(ctx, ns, runID, svc, ip); err != nil {
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
// fails before its new proxy pod is made deletes the config Secret it wrote.
func (k *keepDriver) ReplaceProxyHashedAt(ctx context.Context, ref string, cfgJSON []byte, gen int) (string, error) {
	runID, ns, svc, err := k.runService(ctx, ref)
	if err != nil {
		return "", err
	}
	old, err := k.proxyNotNewer(ctx, ns, runID, gen, true)
	if err != nil {
		return "", err
	}
	in, err := k.replaceInputs(ctx, ns, runID, cfgJSON)
	if err != nil {
		return "", err
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
	if _, err := k.clientset.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("k8s: keep: create the new proxy config Secret: %w", err)
	}
	in.SecretName = sec.Name
	if err := k.swapProxy(ctx, ns, runID, svc, in, old, gen); err != nil {
		return "", err
	}
	return proxy.ConfigSHA256(cfgJSON), nil
}
