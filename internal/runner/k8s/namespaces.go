// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A runner may place runs in several namespaces (design T5 §2.7), the first being the default. All
// of one run's objects live in one of them. With one namespace every lookup below answers it without
// a read, so a single-namespace driver behaves as it always has.
var (
	// errRunsNamespaceNotServed: a create named a namespace this driver does not place runs in.
	errRunsNamespaceNotServed = errors.New("runs_namespace_not_served")
	// errRunsNamespaceUnlisted: a live run's record names a namespace no longer listed at boot.
	errRunsNamespaceUnlisted = errors.New("runs_namespace_unlisted")
)

// defaultNamespace is the first namespace, where a spec naming none is created. A Driver built
// without New (a unit test) may have none.
func (d *Driver) defaultNamespace() string {
	if len(d.cfg.Namespaces) == 0 {
		return ""
	}
	return d.cfg.Namespaces[0]
}

// inNamespace names ns in err when the driver has several namespaces; with one, err is unchanged.
func (d *Driver) inNamespace(ns string, err error) error {
	if len(d.cfg.Namespaces) <= 1 {
		return err
	}
	return fmt.Errorf("namespace %s: %w", ns, err)
}

// createNamespace is the namespace spec's run is created in: the one it names, refused when this
// driver does not serve it, else the default. Nothing is created before this answer.
func (d *Driver) createNamespace(spec runner.SandboxSpec) (string, error) {
	if spec.Namespace == "" {
		return d.defaultNamespace(), nil
	}
	if !slices.Contains(d.cfg.Namespaces, spec.Namespace) {
		return "", fmt.Errorf("%w: this runner does not place runs in namespace %q (it serves %s)", errRunsNamespaceNotServed, spec.Namespace, strings.Join(d.cfg.Namespaces, ", "))
	}
	return spec.Namespace, nil
}

// noteNamespace records the namespace a run's objects live in. One namespace needs no record.
func (d *Driver) noteNamespace(runID uuid.UUID, ns string) {
	if len(d.cfg.Namespaces) > 1 {
		d.runNamespaces.Store(runID, ns)
	}
}

// notedNamespace is the namespace noted for runID, else the default: no read.
func (d *Driver) notedNamespace(runID uuid.UUID) string {
	if v, ok := d.runNamespaces.Load(runID); ok {
		return v.(string)
	}
	return d.defaultNamespace()
}

// runNamespace is the namespace run runID's objects live in: the noted one, else the one its record
// names (runner mode), else the first namespace holding a pod of the run. A run found nowhere
// answers the default namespace, where every read then finds nothing, as with one namespace.
func (d *Driver) runNamespace(ctx context.Context, runID uuid.UUID) (string, error) {
	if len(d.cfg.Namespaces) <= 1 {
		return d.defaultNamespace(), nil
	}
	if v, ok := d.runNamespaces.Load(runID); ok {
		return v.(string), nil
	}
	if d.cfg.Evidence != nil {
		ns, err := d.recordedNamespace(runID)
		if err != nil {
			return "", err
		}
		if ns != "" {
			d.noteNamespace(runID, ns)
			return ns, nil
		}
	}
	sel := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String()}
	for _, ns := range d.cfg.Namespaces {
		pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, sel)
		if err != nil {
			return "", fmt.Errorf("k8s: find run %s's namespace: list pods in %s: %w", runID, ns, err)
		}
		if len(pods.Items) > 0 {
			d.noteNamespace(runID, ns)
			return ns, nil
		}
	}
	return d.defaultNamespace(), nil
}

// refNamespace is runNamespace for a sandbox ref (the agent pod's name). A ref naming no run answers
// the default namespace.
func (d *Driver) refNamespace(ctx context.Context, ref string) (string, error) {
	id, err := runIDFromAgentPodName(ref)
	if err != nil {
		return d.defaultNamespace(), nil
	}
	return d.runNamespace(ctx, id)
}

// recordedNamespace is the namespace the run's record names ("" for none). It reads without the
// run's lock, so a caller holding it may ask.
func (d *Driver) recordedNamespace(runID uuid.UUID) (string, error) {
	b, err := d.cfg.Evidence.LoadEvidence(runID)
	if err != nil {
		return "", fmt.Errorf("k8s: load run evidence: %w", err)
	}
	ev, err := decodeEvidence(b)
	if err != nil {
		return "", fmt.Errorf("k8s: decode run evidence: %w", err)
	}
	return ev.Namespace, nil
}

// CheckRunNamespaces is the runner's boot pass over its namespaces. It refuses boot when a live run
// (runIDs: the state directory's unretired runs) is recorded in a namespace no longer listed, since
// runnerd could no longer stop its pods, then notes each listed namespace's runs from this runner's
// pods there.
func (d *Driver) CheckRunNamespaces(ctx context.Context, runIDs []uuid.UUID) error {
	var unlisted []string
	for _, id := range runIDs {
		ns, err := d.recordedNamespace(id)
		if err != nil {
			return err
		}
		if ns != "" && !slices.Contains(d.cfg.Namespaces, ns) {
			unlisted = append(unlisted, id.String()+" in "+ns)
		}
	}
	if len(unlisted) > 0 {
		return fmt.Errorf("%w: live runs are in a namespace no longer listed; list it again with --runs-namespace until they end: %s", errRunsNamespaceUnlisted, strings.Join(unlisted, ", "))
	}
	if len(d.cfg.Namespaces) <= 1 {
		return nil
	}
	for _, ns := range d.cfg.Namespaces {
		pods, err := d.clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: d.runnerSelector("")})
		if err != nil {
			return fmt.Errorf("k8s: list runner pods in %s: %w", ns, err)
		}
		for _, p := range pods.Items {
			if id, err := uuid.Parse(p.Labels[labelRun]); err == nil {
				d.noteNamespace(id, ns)
			}
		}
	}
	return nil
}
