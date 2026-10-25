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
	if macVMUpgraded != "This Mac's runner was upgraded while the macOS VM ran" || macVMLowDisk != "This Mac ran low on disk" {
		t.Fatalf("end texts %q, %q", macVMUpgraded, macVMLowDisk)
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
