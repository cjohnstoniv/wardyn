// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"path/filepath"
	"testing"
)

func TestRunnerSpoolOnlyMountsProxyAndRecoversPartialCreate(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	dir := t.TempDir()
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerStateDir: dir})
	spec := testSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Ref != SandboxRef(spec.RunID) {
		t.Fatal("persisted intent cannot recover actual ref")
	}
	proxy := f.containers[proxyContainerName(spec.RunID)]
	agent := f.containers[agentContainerName(spec.RunID)]
	if len(proxy.host.Mounts) != 1 || proxy.host.Mounts[0].Source != filepath.Join(dir, "spools", spec.RunID.String()) || proxy.host.Mounts[0].Target != runnerSpoolTarget || proxy.host.Mounts[0].ReadOnly {
		t.Fatalf("proxy spool mount: %+v", proxy.host.Mounts)
	}
	for _, m := range agent.host.Mounts {
		if m.Target == runnerSpoolTarget || m.Source == proxy.host.Mounts[0].Source {
			t.Fatal("agent has access to audit spool")
		}
	}
	// Containment must not require the agent container to have been created.
	delete(f.containers, agentContainerName(spec.RunID))
	if err := d.StopProxy(context.Background(), SandboxRef(spec.RunID)); err != nil {
		t.Fatal(err)
	}
	if !proxy.removed {
		t.Fatal("partial proxy provisioning escaped containment")
	}
}
func TestRunnerSpoolConfigUsesPrivateWriterIdentity(t *testing.T) {
	f := newFakeDocker()
	d := newWithClient(f, Config{RunnerStateDir: t.TempDir()})
	f.info.SecurityOptions = []string{"name=rootless"}
	cfg, host := &container.Config{}, &container.HostConfig{}
	data, err := d.runnerSpoolConfig(context.Background(), uuid.New(), []byte(`{"existing":"kept"}`), cfg, host)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["existing"] != "kept" || fields["decision_spool_dir"] != runnerSpoolTarget || cfg.User != "0:0" {
		t.Fatalf("config/identity: %s user%s", data, cfg.User)
	}
}

// A spool path in the received config never survives: the runner writes its own.
func TestRunnerSpoolConfigReplacesAReceivedSpoolDir(t *testing.T) {
	f := newFakeDocker()
	d := newWithClient(f, Config{RunnerStateDir: t.TempDir()})
	data, err := d.runnerSpoolConfig(context.Background(), uuid.New(), []byte(`{"decision_spool_dir":"/org/chosen/spool"}`), &container.Config{}, &container.HostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["decision_spool_dir"] != runnerSpoolTarget {
		t.Fatalf("decision_spool_dir = %q, want the runner's own %q", fields["decision_spool_dir"], runnerSpoolTarget)
	}
}
