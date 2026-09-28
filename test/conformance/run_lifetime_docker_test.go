// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

// run_lifetime_docker_test.go exercises RL-1..12's run-lifetime transitions
// (long-holds design rev 4 §4) against a REAL docker daemon through the exact
// interface the control plane drives — the orchestrator over the docker
// substrate, with the SAME optional-interface type assertions
// internal/api/run_lease.go, run_lost.go and run_revive.go make
// (s.cfg.Runner.(runner.SandboxEnder) and siblings). This is one level above
// internal/runner/docker's own driver_proxy_revive_test.go, which proves the
// SAME driver methods (EndSandbox, StopProxy, ReplaceProxy, StartSandbox)
// against an in-memory fake Docker API, never a real container.
//
// Egress is proven STRUCTURALLY: the proxy sidecar is the sandbox's ONLY
// route off its gatewayless per-run network (CreateSandbox's Internal=true
// network — see driver_network.go), so a stopped or removed proxy container
// IS "no egress" here, the same structural style testL0StructuralEgress
// already uses. This suite does not stand up a functional egress relay —
// neither does conformance_docker_test.go's own busybox stand-in proxy (see
// its ProxyImage comment) — so no HTTP allow/deny control is exercised.
//
// Guarded by WARDYN_TEST_DOCKER=1, exactly like TestConformanceDocker.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runLifetimeCleanupTimeout mirrors conformance.go's own unexported
// conformanceCleanupTimeout: a post-verdict teardown must not spend the
// package's remaining -timeout on a substrate that is itself slow or wedged.
const runLifetimeCleanupTimeout = 60 * time.Second

// runLifetimeSpec mirrors conformance.go's own unexported minimalSpec (this
// file is package conformance_test and cannot reach it): a bare SandboxSpec
// for busybox with a fresh RunID.
func runLifetimeSpec(image string, class types.ConfinementClass) runner.SandboxSpec {
	return runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            image,
		ConfinementClass: class,
		Labels:           map[string]string{"wardyn.conformance": "true"},
	}
}

func TestRunLifetimeDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping docker conformance")
	}

	sub, err := docker.New(docker.Config{
		// See TestConformanceDocker's identical comment: busybox stands in for
		// the real wardyn-proxy image so this gate needs no proxy binary. It
		// echoes the config it is handed on stdin (#1176: its only way in) to
		// its log, which is how the revive case reads what each proxy got (the
		// echo ends the line: the log driver holds a partial one back).
		ProxyImage: "busybox:latest",
		ProxyCmd:   []string{"sh", "-c", "cat; echo; exec sleep infinity"},
	})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	// Held as the runner.Runner interface, like the api layer holds
	// s.cfg.Runner: every lifecycle call below type-asserts the SAME optional
	// interfaces run_lease.go/run_lost.go/run_revive.go do.
	var r runner.Runner = orchestrator.New(sub)
	ensureConformanceNetwork(t, "wardyn-internal")

	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skip("driver advertises no confinement classes")
	}
	class := caps.ConfinementClasses[len(caps.ConfinementClasses)-1]

	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	inspect := func(t *testing.T, name string) (running, paused bool, ok bool) {
		t.Helper()
		res, err := cli.ContainerInspect(context.Background(), name, dockerclient.ContainerInspectOptions{})
		if err != nil {
			return false, false, false
		}
		if res.Container.State == nil {
			t.Fatalf("inspect %s: no State", name)
		}
		return res.Container.State.Running, res.Container.State.Paused, true
	}
	requireState := func(t *testing.T, name string, wantRunning, wantPaused bool) {
		t.Helper()
		running, paused, ok := inspect(t, name)
		if !ok {
			t.Fatalf("inspect %s: container not found; want it to exist (running=%v paused=%v)", name, wantRunning, wantPaused)
		}
		if running != wantRunning || paused != wantPaused {
			t.Errorf("%s: running=%v paused=%v, want running=%v paused=%v", name, running, paused, wantRunning, wantPaused)
		}
	}
	requireGone := func(t *testing.T, name string) {
		t.Helper()
		if _, _, ok := inspect(t, name); ok {
			t.Errorf("%s still exists; want it removed", name)
		}
	}
	// newSandbox creates a fresh agent+proxy pair, both running, and registers
	// teardown. proxyRef is the deterministic name naming.go's proxyContainerName
	// computes ("wardyn-proxy-"+runID) — unexported, so reproduced here from the
	// RunID this test controls.
	newSandbox := func(t *testing.T) (agentRef, proxyRef string) {
		t.Helper()
		spec := runLifetimeSpec("busybox:latest", class)
		spec.ProxyConfig.MITMCACertPEM = "test-mitm-ca-" + spec.RunID.String()
		sb, err := r.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		agentRef = sb.Ref
		proxyRef = "wardyn-proxy-" + spec.RunID.String()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), runLifetimeCleanupTimeout)
			defer cancel()
			_ = r.StopSandbox(ctx, agentRef)
		})
		requireState(t, agentRef, true, false)
		requireState(t, proxyRef, true, false)
		return agentRef, proxyRef
	}

	// EndKeepsAgentAndCutsEgress: RL-9/run_lease.go's lease end (endSandbox).
	// The agent container is KEPT (never removed — its writable layer is the
	// checkout/transcript a kept run is kept FOR) but its process is stopped,
	// and the proxy is removed (a revive rebuilds it from the control plane's
	// stored config, #1176), so the sandbox's only egress path is cut.
	t.Run("EndKeepsAgentAndCutsEgress", func(t *testing.T) {
		agentRef, proxyRef := newSandbox(t)

		ender, ok := r.(runner.SandboxEnder)
		if !ok {
			t.Fatal("orchestrator does not implement runner.SandboxEnder")
		}
		if err := ender.EndSandbox(context.Background(), agentRef); err != nil {
			t.Fatalf("EndSandbox: %v", err)
		}

		requireState(t, agentRef, false, false) // kept, not removed — but stopped
		requireGone(t, proxyRef)
	})

	// LostProxyCutsEgress: RL-9/run_lost.go's outage-inside-the-lease branch
	// (stopLostSandbox). Only the proxy is touched; the agent keeps running —
	// this is what distinguishes an outage from an end.
	t.Run("LostProxyCutsEgress", func(t *testing.T) {
		agentRef, proxyRef := newSandbox(t)

		stopper, ok := r.(runner.ProxyStopper)
		if !ok {
			t.Fatal("orchestrator does not implement runner.ProxyStopper")
		}
		if err := stopper.StopProxy(context.Background(), agentRef); err != nil {
			t.Fatalf("StopProxy: %v", err)
		}

		requireState(t, agentRef, true, false) // an outage inside the lease keeps the agent running
		requireGone(t, proxyRef)
	})

	// ReviveReusesAddressAndCA: RL-9's proxy-only revive (runner.ProxyReviver).
	// The new proxy must land at the SAME address the agent's hosts entry
	// pins, and receive, on a real engine's stdin, exactly the config the
	// control plane hands ReplaceProxy: the MITM CA carried over, only the
	// token rewritten (ProxyReviver's doc). Neither proxy's container config
	// may hold the CA (#1176).
	t.Run("ReviveReusesAddressAndCA", func(t *testing.T) {
		agentRef, proxyRef := newSandbox(t)
		proxyIP := func(t *testing.T) string {
			t.Helper()
			res, err := cli.ContainerInspect(context.Background(), proxyRef, dockerclient.ContainerInspectOptions{})
			if err != nil {
				t.Fatalf("inspect proxy: %v", err)
			}
			// The proxy joins TWO networks (the per-run internal one, where its
			// PINNED address lives, and the control-plane-facing "wardyn-internal"
			// one) — map iteration order is randomized, so picking "whichever
			// comes first" flakes between the two. internalNetName's own name
			// ("wardyn-int-"+runID) is unexported; its prefix is distinct from
			// the literal "wardyn-internal" ("wardyn-int-" has a trailing hyphen
			// "wardyn-internal" does not), so it still selects the right one
			// without reaching into the docker package.
			for name, ep := range res.Container.NetworkSettings.Networks {
				if strings.HasPrefix(name, "wardyn-int-") && ep.IPAddress.IsValid() {
					return ep.IPAddress.String()
				}
			}
			t.Fatal("proxy container has no IP on its per-run internal network")
			return ""
		}
		oldIP := proxyIP(t)

		reviver, ok := r.(runner.ProxyReviver)
		if !ok {
			t.Fatal("orchestrator does not implement runner.ProxyReviver")
		}
		stopper, ok := r.(runner.ProxyStopper)
		if !ok {
			t.Fatal("orchestrator does not implement runner.ProxyStopper")
		}
		oldCfg := receivedProxyConfig(t, cli, proxyRef)
		if err := stopper.StopProxy(context.Background(), agentRef); err != nil {
			t.Fatalf("StopProxy: %v", err)
		}
		requireGone(t, proxyRef)

		var old struct {
			RunToken      string `json:"run_token"`
			MITMCACertPEM string `json:"mitm_ca_cert_pem"`
		}
		if err := json.Unmarshal([]byte(oldCfg), &old); err != nil {
			t.Fatalf("decode old proxy config: %v", err)
		}
		if old.MITMCACertPEM == "" {
			t.Fatal("the CA this test set at create never round-tripped into the rendered proxy config; the assertion below would prove nothing")
		}

		fresh := strings.Replace(oldCfg, `"run_token":"`+old.RunToken+`"`, `"run_token":"revived-token"`, 1)
		if fresh == oldCfg {
			t.Fatal("could not rewrite run_token in the captured config; the revive below would prove nothing")
		}
		if err := reviver.EnsureProxyImage(context.Background()); err != nil {
			t.Fatalf("EnsureProxyImage: %v", err)
		}
		if err := reviver.ReplaceProxy(context.Background(), agentRef, []byte(fresh)); err != nil {
			t.Fatalf("ReplaceProxy: %v", err)
		}

		// The engine hands a freed address back to the next unpinned endpoint,
		// so this proves a real engine accepts the pinned create (#1133), not
		// that the pin is asked for: TestReplaceProxy_ALostRunsProxyComesBackAtItsAddress does.
		if got := proxyIP(t); got != oldIP {
			t.Errorf("revived proxy address = %s, want the SAME address the agent's hosts entry pins (%s)", got, oldIP)
		}
		requireState(t, proxyRef, true, false)

		newCfg := receivedProxyConfig(t, cli, proxyRef)
		if newCfg != fresh {
			t.Errorf("revived proxy received %q on stdin, want exactly the config handed to ReplaceProxy %q", newCfg, fresh)
		}
		var neu struct {
			RunToken      string `json:"run_token"`
			MITMCACertPEM string `json:"mitm_ca_cert_pem"`
		}
		if err := json.Unmarshal([]byte(newCfg), &neu); err != nil {
			t.Fatalf("decode revived proxy config: %v", err)
		}
		if neu.RunToken != "revived-token" {
			t.Errorf("revived run_token = %q, want the rewritten one", neu.RunToken)
		}
		if neu.MITMCACertPEM != old.MITMCACertPEM {
			t.Errorf("revived MITM CA = %q, want the SAME CA carried over verbatim (%q) — a revive must never mint or copy a new one",
				neu.MITMCACertPEM, old.MITMCACertPEM)
		}
		res, err := cli.ContainerInspect(context.Background(), proxyRef, dockerclient.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect revived proxy: %v", err)
		}
		if b, _ := json.Marshal(res.Container.Config); strings.Contains(string(b), old.MITMCACertPEM) {
			t.Errorf("the revived proxy's container config holds the MITM CA; it must arrive only on stdin (#1176): %s", b)
		}

		// The agent was never stopped in this scenario (StopProxy alone keeps
		// it running); StartSandbox behind the now-running proxy must still
		// succeed as a no-op-on-already-running call.
		starter, ok := r.(runner.SandboxStarter)
		if !ok {
			t.Fatal("orchestrator does not implement runner.SandboxStarter")
		}
		if err := starter.StartSandbox(context.Background(), agentRef); err != nil {
			t.Fatalf("StartSandbox: %v", err)
		}
		requireState(t, agentRef, true, false)
	})

	// FreezeAgentOnlyAndInspectExec: RL-0's pause spike. FreezeSandbox pauses
	// ONLY the agent — the proxy keeps running, renewing and answering egress
	// decisions — and a paused container refuses a new exec (verified against
	// runc/cgroup v2 per FreezeSandbox's own doc comment).
	t.Run("FreezeAgentOnlyAndInspectExec", func(t *testing.T) {
		agentRef, proxyRef := newSandbox(t)

		freezer, ok := r.(runner.Freezer)
		if !ok {
			t.Fatal("orchestrator does not implement runner.Freezer")
		}
		// A class whose runtime cannot be paused safely (runsc, Kata: see
		// Capabilities.Freeze) must be REFUSED, and the agent left running —
		// never paused on a runtime nobody verified the pause against.
		if !caps.Freeze[class] {
			if err := freezer.FreezeSandbox(context.Background(), agentRef); !errors.Is(err, runner.ErrFreezeUnsupported) {
				t.Fatalf("FreezeSandbox on %s (Capabilities.Freeze=false) = %v, want runner.ErrFreezeUnsupported", class, err)
			}
			requireState(t, agentRef, true, false)
			requireState(t, proxyRef, true, false)
			return
		}
		if err := freezer.FreezeSandbox(context.Background(), agentRef); err != nil {
			t.Fatalf("FreezeSandbox: %v", err)
		}
		requireState(t, agentRef, true, true)  // paused, not stopped
		requireState(t, proxyRef, true, false) // untouched

		// ExecCreate merely registers the exec; a paused container's refusal
		// can surface there OR at attach/start, so accept either — the
		// invariant is that SOME exec attempt against the frozen agent fails.
		execID, createErr := cli.ExecCreate(context.Background(), agentRef, dockerclient.ExecCreateOptions{
			Cmd: []string{"true"}, AttachStdout: true, AttachStderr: true,
		})
		if createErr == nil {
			attachRes, attachErr := cli.ExecAttach(context.Background(), execID.ID, dockerclient.ExecAttachOptions{})
			if attachErr == nil {
				attachRes.Close()
				t.Error("an exec against the PAUSED agent completed with no error; want it refused")
			}
		}

		if err := freezer.ThawSandbox(context.Background(), agentRef); err != nil {
			t.Fatalf("ThawSandbox: %v", err)
		}
		requireState(t, agentRef, true, false)

		liveID, err := cli.ExecCreate(context.Background(), agentRef, dockerclient.ExecCreateOptions{
			Cmd: []string{"true"}, AttachStdout: true, AttachStderr: true,
		})
		if err != nil {
			t.Fatalf("ExecCreate on the thawed agent: %v", err)
		}
		attachRes, err := cli.ExecAttach(context.Background(), liveID.ID, dockerclient.ExecAttachOptions{})
		if err != nil {
			t.Fatalf("ExecAttach on the thawed agent: %v", err)
		}
		attachRes.Close()

		deadline := time.Now().Add(5 * time.Second)
		var insp dockerclient.ExecInspectResult
		for {
			insp, err = cli.ExecInspect(context.Background(), liveID.ID, dockerclient.ExecInspectOptions{})
			if err != nil {
				t.Fatalf("ExecInspect: %v", err)
			}
			if !insp.Running || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if insp.Running || insp.ExitCode != 0 {
			t.Errorf("exec on the thawed agent = running=%v exitCode=%d, want it to have completed with 0", insp.Running, insp.ExitCode)
		}
	})
}

// receivedProxyConfig is the config the stand-in proxy (ProxyCmd above) read
// on stdin and echoed to its log.
func receivedProxyConfig(t *testing.T, cli *dockerclient.Client, proxyRef string) string {
	t.Helper()
	return strings.TrimSpace(waitForLog(t, cli, proxyRef, `"run_token"`, 30*time.Second))
}
