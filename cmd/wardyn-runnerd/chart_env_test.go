// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// TestChartSetsOnlyWhatRunnerdReads renders deploy/helm/wardyn-runnerd with every optional setting
// on and holds the Deployment to runnerd: each flag the init container and the runnerd container
// pass parses on the register and serve commands, each environment variable is one runnerd reads
// (k8sEnvRead), and the rendered values pass readK8sEnv.
func TestChartSetsOnlyWhatRunnerdReads(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; make helm-lint runs this chart's own render test")
	}
	chart := filepath.Join("..", "..", "deploy", "helm", "wardyn-runnerd")
	out, err := exec.Command(helm, "template", "r", chart, "-n", "wardyn-runner", "-f", filepath.Join(chart, "ci", "test-values.yaml"),
		"--set", "apiServer.peers[0].ipBlock.cidr=10.0.0.1/32",
		"--set", "egress.hardDenyCIDRs[0]=10.96.0.0/12",
		"--set", "egress.hardDenyCIDRs[1]=10.244.0.0/16",
		"--set", "k8s.imagePullSecret=pull",
		"--set", "k8s.sandbox.nodeSelector.pool=runs",
		"--set", "k8s.runtimeClasses.CC2=runsc",
		"--set", "k8s.linuxVM=true",
		"--set", "trustedCA.configMap=corp-ca",
		"--set", "limits.maxRuns=2", "--set", "limits.totalCPU=4", "--set", "limits.totalMemory=8", "--set", "limits.totalDisk=50",
		"--set", "limits.perRunCPU=2", "--set", "limits.perRunMemory=4", "--set", "limits.perRunDisk=25",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var dep *appsv1.Deployment
	for _, doc := range strings.Split(string(out), "\n---\n") {
		if !strings.Contains(doc, "\nkind: Deployment\n") {
			continue
		}
		dep = &appsv1.Deployment{}
		if err := yaml.Unmarshal([]byte(doc), dep); err != nil {
			t.Fatalf("decode Deployment: %v", err)
		}
	}
	if dep == nil {
		t.Fatal("the chart rendered no Deployment")
	}
	spec := dep.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("want one init container and one container, got %d and %d", len(spec.InitContainers), len(spec.Containers))
	}
	checkArgs := func(c corev1.Container, verb string) {
		t.Helper()
		if len(c.Args) == 0 || c.Args[0] != verb {
			t.Fatalf("%s: args %v do not start with %q", c.Name, c.Args, verb)
		}
		cmd := rootCommand()
		sub, _, err := cmd.Find([]string{verb})
		if err != nil {
			t.Fatal(err)
		}
		if err := sub.ParseFlags(c.Args[1:]); err != nil {
			t.Errorf("%s: runnerd %s does not take the chart's flags: %v", c.Name, verb, err)
		}
		if rest := sub.Flags().Args(); len(rest) > 0 {
			t.Errorf("%s: arguments runnerd would refuse: %v", c.Name, rest)
		}
	}
	checkArgs(spec.InitContainers[0], "register")
	checkArgs(spec.Containers[0], "serve")
	// The volume's root is group-writable, never 0700, so state lives in a directory below it; and the
	// registration call trusts the corporate CA bundle the serve container does.
	for _, c := range []corev1.Container{spec.InitContainers[0], spec.Containers[0]} {
		var stateDir string
		for _, a := range c.Args {
			if v, ok := strings.CutPrefix(a, "--state-dir="); ok {
				stateDir = v
			}
		}
		var mount string
		for _, m := range c.VolumeMounts {
			if m.Name == "state" {
				mount = m.MountPath
			}
		}
		if !strings.HasPrefix(stateDir, mount+"/") {
			t.Errorf("%s: --state-dir %q is not a directory below the state volume %q", c.Name, stateDir, mount)
		}
	}
	if !slices.Contains(spec.InitContainers[0].Args, "--trusted-ca-file=/etc/wardyn/trusted-ca/ca.pem") {
		t.Errorf("the register container does not trust the corporate CA: %v", spec.InitContainers[0].Args)
	}
	if !slices.ContainsFunc(spec.InitContainers[0].VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == "/etc/wardyn/trusted-ca" }) {
		t.Error("the register container does not mount the corporate CA bundle")
	}
	if len(spec.InitContainers[0].Env) > 0 {
		t.Errorf("the register container sets environment runnerd register does not read: %v", spec.InitContainers[0].Env)
	}
	var environ []string
	for _, e := range spec.Containers[0].Env {
		if !slices.Contains(k8sEnvRead, e.Name) {
			t.Errorf("the chart sets %s, which runnerd does not read", e.Name)
		}
		v := e.Value
		if e.Name == "POD_IP" {
			if e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "status.podIP" {
				t.Errorf("POD_IP is not the downward API's status.podIP")
			}
			v = "10.1.2.3"
		}
		environ = append(environ, e.Name+"="+v)
	}
	if _, err := readK8sEnv(environ); err != nil {
		t.Errorf("runnerd refuses the chart's rendered settings: %v", err)
	}
}
