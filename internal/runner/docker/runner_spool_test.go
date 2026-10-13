// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
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

// A root runnerd on a rootful daemon never runs the proxy as root: the proxy keeps its image's non-root
// user and the spool is handed to it; a non-root runnerd's proxy runs as runnerd.
func TestRunnerSpoolProxyIsNeverRootOnARootfulDaemon(t *testing.T) {
	var handed []string
	uid := 0
	processUID, chownSpool = func() int { return uid }, func(dir string, u, g int) error {
		handed = append(handed, fmt.Sprintf("%s %d:%d", filepath.Base(dir), u, g))
		return nil
	}
	t.Cleanup(func() { processUID, chownSpool = os.Getuid, chownSpoolTree })
	d := newWithClient(newFakeDocker(), Config{RunnerStateDir: t.TempDir()})
	id := uuid.New()
	cfg := &container.Config{}
	if _, err := d.runnerSpoolConfig(context.Background(), id, []byte(`{}`), cfg, &container.HostConfig{}); err != nil {
		t.Fatal(err)
	}
	if cfg.User != "65532:65532" || len(handed) != 1 || handed[0] != id.String()+" 65532:65532" {
		t.Fatalf("root runnerd: proxy user %q, spool handed %v; want the image's non-root user and the spool handed to it", cfg.User, handed)
	}
	uid, handed, cfg = 1000, nil, &container.Config{}
	if _, err := d.runnerSpoolConfig(context.Background(), uuid.New(), []byte(`{}`), cfg, &container.HostConfig{}); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); cfg.User != want || handed != nil {
		t.Fatalf("non-root runnerd: proxy user %q, handed %v; want %q and no hand-over", cfg.User, handed, want)
	}
}

// chownSpoolTree hands the directory and its files over without following a link out of it.
func TestChownSpoolTreeStaysInTheSpool(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// To this process's own uid, which needs no privilege: every entry is reached and none errors.
	if err := chownSpoolTree(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
}
