// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pinsProxy reports whether any container f created (or the exec-less agent d holds back) pins
// "wardyn-proxy" to addr in its hosts.
func pinsProxy(f *fakeDocker, d *Driver, addr string) bool {
	want := "wardyn-proxy:" + addr
	f.mu.Lock()
	for _, c := range f.containers {
		if c.host != nil && slices.Contains(c.host.ExtraHosts, want) {
			f.mu.Unlock()
			return true
		}
	}
	f.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.pending {
		if p.host != nil && slices.Contains(p.host.ExtraHosts, want) {
			return true
		}
	}
	return false
}

// checkCreateHash fails unless sb carries the hash of exactly the bytes written to runID's proxy
// stdin, and the proxy address the agent (or the VM's relay) pins.
func checkCreateHash(t *testing.T, f *fakeDocker, d *Driver, runID uuid.UUID, sb runner.Sandbox) []byte {
	t.Helper()
	written := f.stdinOf(proxyContainerName(runID))
	if len(written) == 0 || sb.ProxyConfigSHA256 != proxy.ConfigSHA256(written) {
		t.Fatalf("create hash %q, want the hash of the %d bytes written to the proxy", sb.ProxyConfigSHA256, len(written))
	}
	if sb.ProxyAddr == "" || !pinsProxy(f, d, sb.ProxyAddr) {
		t.Fatalf("create proxy address %q is not the one pinned as wardyn-proxy", sb.ProxyAddr)
	}
	return written
}

// A runner's create with the decision sink on: the hash is of the bytes the stdin write sent, after
// the sink key and the run-network listen address went in, not of the built or prepared config; the
// listen it covers is the proxy's own address; a revive answers the hash of its own write. A policy
// with workspace secret values hashes the same way.
func TestProxyConfigHash_RunnerSinkOnCreateAndRevive(t *testing.T) {
	for _, secrets := range []bool{false, true} {
		t.Run("workspace secrets "+strconv.FormatBool(secrets), func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal",
				RunnerID: "runner-1", DecisionSink: newFakeSink(), Evidence: &memEvidence{m: map[uuid.UUID][]byte{}}})
			ctx := context.Background()
			spec := testSpec()
			if secrets {
				spec.ProxyConfig.Policy.LLMInspection = &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"s3cr3t-value"}}
			}
			sb, err := d.CreateSandbox(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			written := checkCreateHash(t, f, d, spec.RunID, sb)

			built, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			_, proxyCfg, err := proxyEnv(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
			if err != nil {
				t.Fatal(err)
			}
			prep, err := d.prepareProxy(ctx, spec.RunID, proxyCfg, false)
			if err != nil {
				t.Fatal(err)
			}
			if sb.ProxyConfigSHA256 == proxy.ConfigSHA256(built) || sb.ProxyConfigSHA256 == proxy.ConfigSHA256(prep.cfgJSON) {
				t.Fatal("the create hash is of the built or prepared config, not of the bytes written")
			}

			var m map[string]json.RawMessage
			if err := json.Unmarshal(written, &m); err != nil {
				t.Fatal(err)
			}
			if _, ok := m["decision_sink"]; !ok {
				t.Fatal("the written config has no decision_sink: this is not the runner-mode write")
			}
			if string(m["listen"]) != strconv.Quote(net.JoinHostPort(sb.ProxyAddr, strconv.Itoa(runner.ProxyListenPort))) {
				t.Fatalf("written listen %s, want the proxy's run-network address %s", m["listen"], sb.ProxyAddr)
			}
			var before map[string]json.RawMessage
			_ = json.Unmarshal(prep.cfgJSON, &before)
			m["listen"] = before["listen"]
			unpinned, _ := json.Marshal(m)
			if sb.ProxyConfigSHA256 == proxy.ConfigSHA256(unpinned) {
				t.Fatal("the create hash does not cover the pinned listen address")
			}

			fresh := renderedTestConfig(t, "fresh")
			sum, err := d.ReplaceProxyHashed(ctx, sb.Ref, fresh)
			if err != nil {
				t.Fatal(err)
			}
			revived := f.stdinOf(proxyContainerName(spec.RunID))
			if sum != proxy.ConfigSHA256(revived) || sum == proxy.ConfigSHA256(fresh) || sum == sb.ProxyConfigSHA256 {
				t.Fatalf("revive hash %q, want the hash of its own written bytes", sum)
			}
		})
	}
}

// A first proxy refused for allowed_peers and started again without them: the hash is the retry's
// written bytes, which carry no allowed_peers.
func TestProxyConfigHash_AllowedPeersFallbackHashesTheRetry(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	f.olderProxyRefuses = "allowed_peers"
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:0.9.8", InternalNetwork: "wardyn-internal", ProxyPeersOnlyByDefault: true})
	spec := testSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	written := checkCreateHash(t, f, d, spec.RunID, sb)
	if got := allowedPeersOf(t, written); got != nil {
		t.Fatalf("the written config still has allowed_peers %v: not the retry's", got)
	}
	attempts := 0
	f.mu.Lock()
	for _, a := range f.attached {
		if a.id == proxyContainerName(spec.RunID) {
			attempts++
		}
	}
	f.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("%d proxy attaches, want the refused one and the retry", attempts)
	}
}

// Every Docker create returns the hash and the proxy address: the exec-less container, the
// container, the Linux VM and the macOS VM.
func TestProxyConfigHash_EveryCreateReturnsIt(t *testing.T) {
	t.Run("container", func(t *testing.T) {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
		spec := testSpec()
		sb, err := d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		checkCreateHash(t, f, d, spec.RunID, sb)
	})
	t.Run("exec-less", func(t *testing.T) {
		f := newFakeDocker()
		f.info = infoWithRuntimes("krun")
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev"})
		spec := testSpec()
		spec.ConfinementClass = types.CC3
		sb, err := d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		checkCreateHash(t, f, d, spec.RunID, sb)
	})
	t.Run("linux vm", func(t *testing.T) {
		r := newVMRig(t)
		spec := vmSpec()
		sb, err := r.d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		checkCreateHash(t, r.f, r.d, spec.RunID, sb)
	})
	t.Run("macos vm", func(t *testing.T) {
		r := newMacRig(t)
		spec := macSpec()
		sb, err := r.d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		checkCreateHash(t, r.f.fakeDocker, r.d, spec.RunID, sb)
	})
}
