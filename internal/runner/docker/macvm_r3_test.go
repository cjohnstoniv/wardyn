// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// makeSockResidue makes runID's socket directory as its helper would, as a crash leaves it.
func (r *macRig) makeSockResidue(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	sc := macvm.SockConfig(r.c.SockRoot)
	sc.Fstat = macvmtest.SockFstat
	dir, err := sc.MakeRunDir(macvm.RunKey(runID.String()))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// ERRATA E1: a run's sockets are dialled in its socket directory under the socket root, never in its run
// directory, and only once that directory checks out as this login's (0700): one with another mode is
// refused by name, and nothing is dialled there.
func TestMacVMDialsOnlyItsCheckedSocketDir(t *testing.T) {
	r := newMacRig(t)
	spec := macSpec()
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	sockDir := r.c.sockDir(spec.RunID)
	if !strings.HasPrefix(sockDir, r.c.SockRoot+"/u") {
		t.Fatalf("socket dir %s is not under the socket root %s", sockDir, r.c.SockRoot)
	}
	for _, name := range []string{macvm.ControlSock, macvm.AgentSock} {
		if _, err := os.Lstat(filepath.Join(sockDir, name)); err != nil {
			t.Fatalf("%s is not in the socket dir: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(r.c.runDir(spec.RunID), name)); !os.IsNotExist(err) {
			t.Fatalf("%s is in the run dir: %v", name, err)
		}
	}
	if err := os.Chmod(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var refusal *admission.SockRefusal
	if _, err := r.c.dialControl(context.Background(), spec.RunID); !errors.As(err, &refusal) {
		t.Fatalf("dial in a 0755 socket dir: %v, want the named refusal", err)
	}
	if _, err := r.d.DialDesktop(context.Background(), sb.Ref); !errors.As(err, &refusal) {
		t.Fatalf("desktop in a 0755 socket dir: %v, want the named refusal", err)
	}
	_ = os.Chmod(sockDir, 0o700)
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
}

// EnableMacVM's 103-byte guard measures the socket directory, not the run directory: a deep VM
// directory is fine, a socket root too long for desktop.sock is refused.
func TestMacVMSocketGuardMeasuresTheSocketDir(t *testing.T) {
	r := newMacRig(t)
	c := *r.c
	c.Layout = macvm.Layout{Root: filepath.Join(r.c.Layout.Root, strings.Repeat("d", 120))}
	if err := r.d.EnableMacVM(&c); err != nil {
		t.Fatalf("a deep VM directory with a short socket root: %v", err)
	}
	c = *r.c
	c.SockRoot = filepath.Join(r.c.SockRoot, strings.Repeat("s", 80))
	if err := r.d.EnableMacVM(&c); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("a socket root too long for its sockets: %v", err)
	}
}

// A teardown removes the socket directories of this login's runs whose run directory is gone, the one a
// crashed helper left included.
func TestMacVMTeardownSweepsSocketResidue(t *testing.T) {
	r := newMacRig(t)
	sb, err := r.d.CreateSandbox(context.Background(), macSpec())
	if err != nil {
		t.Fatal(err)
	}
	residue := r.makeSockResidue(t, uuid.New()) // after the helper's own start-up sweep
	if _, err := os.Lstat(residue); err != nil {
		t.Fatalf("residue gone before the teardown: %v", err)
	}
	if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(residue); !os.IsNotExist(err) {
		t.Fatalf("a dead run's socket directory outlived the teardown: %v", err)
	}
}

// The run's end texts are the canon's, verbatim (MV-D13, MV-D14; base 3c6ea5e08:176-177).
func TestMacVMEndTextsAreCanon(t *testing.T) {
	if macVMUpgraded != "This Mac's runner was upgraded while the macOS VM ran, so the run was ended." ||
		macVMLowDisk("12\n") != "This Mac ran low on disk (12 GB free), so the macOS VM was stopped." ||
		macVMLowDisk("") != "This Mac ran low on disk, so the macOS VM was stopped." {
		t.Fatalf("end texts %q, %q", macVMUpgraded, macVMLowDisk("12"))
	}
}

// DR-0428: after a run boot, the guest agent's own report of its tools-port dial (GET /v1/dial/tools on
// the 1024 session) is kept for `macos-vm status`.
func TestMacVMRecordsTheRunBootToolsDial(t *testing.T) {
	r := newMacRig(t)
	sb, err := r.d.CreateSandbox(context.Background(), macSpec())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.d.StopSandbox(context.Background(), sb.Ref) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(r.c.ToolsDialFile)
		if strings.TrimSpace(string(b)) == macvm.ToolsDialRefused {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools dial record %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A base job's stage-2 network: an internal network; the prep-only proxy on it, configured on its stdin
// (WARDYN_PROXY_CONFIG_STDIN=1) to serve only that network's subnet and reach only the allowlist's
// hosts on 443; and a relay on stdio with -proxy at it, whose attach is handed back. None of it carries
// a component the orphan sweeps key on, and the undo removes all of it.
func TestMacPrepNetIsTheAllowlistProxyOnly(t *testing.T) {
	r := newMacRig(t)
	const entry = "0123456789abcdef01234567"
	attach, undo, err := r.d.StartMacPrepNet(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	defer attach.Close()
	id := macPrepID(entry)
	netName, proxyName, relayName := internalNetName(id), proxyContainerName(id), vmnetName(id)
	r.f.mu.Lock()
	subnet := r.f.subnets[netName]
	netOpts := r.f.networks[netName]
	px, relay := r.f.containers[proxyName], r.f.containers[relayName]
	r.f.mu.Unlock()
	if !subnet.IsValid() || !netOpts.Internal || px == nil || relay == nil {
		t.Fatalf("network %v internal=%v proxy %v relay %v", subnet, netOpts.Internal, px != nil, relay != nil)
	}
	for _, l := range []map[string]string{netOpts.Labels, px.cfg.Labels, relay.cfg.Labels} {
		if l[labelComponent] != componentMacPrep {
			t.Fatalf("labels %v", l)
		}
	}
	if !slices.Contains(px.cfg.Env, proxyConfigStdinEnv+"=1") || len(px.cfg.Env) != 1 {
		t.Fatalf("proxy env %v", px.cfg.Env)
	}
	cfg, err := proxy.LoadConfigBytes(r.f.stdinOf(proxyName))
	if err != nil {
		t.Fatalf("proxy config on stdin: %v", err)
	}
	if len(cfg.AllowedPeers) != 1 || cfg.AllowedPeers[0] != subnet {
		t.Fatalf("allowed peers %v, want exactly the prep subnet %v", cfg.AllowedPeers, subnet)
	}
	if !slices.Contains(cfg.Policy.AllowedDomains, "swscan.apple.com:443") || len(cfg.Policy.AllowedDomains) != 7 {
		t.Fatalf("allowed %v", cfg.Policy.AllowedDomains)
	}
	want := []string{"-stdio", "-proxy", "wardyn-proxy:" + strconv.Itoa(runner.ProxyListenPort)}
	if !slices.Equal(relay.cfg.Cmd, want) {
		t.Fatalf("relay cmd %v, want %v", relay.cfg.Cmd, want)
	}
	undo()
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if _, ok := r.f.networks[netName]; ok || !px.removed || !relay.removed {
		t.Fatal("the undo left the prep network or a container")
	}
}

// A crashed runnerd's prep proxy, relay and network go at its next start; those of a job whose ledger
// entry is kept, another runner's and another deployment's stay.
func TestMacPrepSweepRemovesACrashsLeftovers(t *testing.T) {
	r := newMacRig(t)
	r.d.cfg.RunnerID = "runner-a"
	ctx := context.Background()
	const dead, live = "0123456789abcdef01234567", "76543210fedcba9876543210"
	for _, e := range []string{dead, live} {
		attach, _, err := r.d.StartMacPrepNet(ctx, e) // its undo never runs: runnerd crashed
		if err != nil {
			t.Fatal(err)
		}
		_ = attach.Close()
	}
	otherRunner := r.d.labels(uuid.New(), componentMacPrep, map[string]string{runner.LabelRunnerID: "runner-b"})
	otherDeployment := r.d.labels(uuid.New(), componentMacPrep, map[string]string{runner.LabelRunnerID: "runner-a"})
	otherDeployment[labelDeployment] = "elsewhere"
	r.f.mu.Lock()
	for name, c := range r.f.containers {
		r.f.listItems = append(r.f.listItems, container.Summary{ID: name, Labels: c.cfg.Labels})
	}
	r.f.listItems = append(r.f.listItems, container.Summary{ID: "other-runner", Labels: otherRunner})
	r.f.networks["other-deployment"] = client.NetworkCreateOptions{Labels: otherDeployment}
	r.f.mu.Unlock()
	n, err := r.d.SweepMacPrep(ctx, []string{live})
	if err != nil || n != 3 {
		t.Fatalf("swept %d, %v; want the dead job's proxy, relay and network", n, err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	for e, gone := range map[string]bool{dead: true, live: false} {
		id := macPrepID(e)
		_, hasNet := r.f.networks[internalNetName(id)]
		if px, relay := r.f.containers[proxyContainerName(id)], r.f.containers[vmnetName(id)]; px.removed != gone || relay.removed != gone || hasNet == gone {
			t.Fatalf("job %s: proxy removed %v, relay removed %v, network kept %v; want removed %v", e, px.removed, relay.removed, hasNet, gone)
		}
	}
	if _, ok := r.f.networks["other-deployment"]; !ok {
		t.Fatal("swept another deployment's prep network")
	}
}

// A runnerd that stopped or crashed after its hand-off leaves the job's stage-2 network: the next one
// adopts it (cmd/wardyn-runnerd handNet) and removes it by the job's names once the helper has gone, and
// one that builds it again first clears what is left, so no name collides on a daemon that refuses a
// name in use.
func TestMacPrepNetNamesNeverCollide(t *testing.T) {
	r := newMacRig(t)
	ctx := context.Background()
	const entry = "0123456789abcdef01234567"
	id := macPrepID(entry)
	inUse := func(name string) bool {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		c := r.f.containers[name]
		_, net := r.f.networks[name]
		return (c != nil && !c.removed) || net
	}
	r.f.onCreate = func(name string) {
		if inUse(name) {
			t.Errorf("container name %s is in use", name)
		}
	}
	r.f.onNetworkCreate = func(name string, _ client.NetworkCreateOptions) {
		if inUse(name) {
			t.Errorf("network name %s is in use", name)
		}
	}
	for range 2 { // the first runnerd's undo never runs
		attach, _, err := r.d.StartMacPrepNet(ctx, entry)
		if err != nil {
			t.Fatal(err)
		}
		_ = attach.Close()
	}
	for range 2 { // removing what is gone is no error
		if err := r.d.RemoveMacPrepNet(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{internalNetName(id), proxyContainerName(id), vmnetName(id)} {
		if inUse(name) {
			t.Fatalf("%s outlived RemoveMacPrepNet", name)
		}
	}
}
