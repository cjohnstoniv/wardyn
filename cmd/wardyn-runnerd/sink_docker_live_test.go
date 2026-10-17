// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDockerProxySendsDecisionsToTheSink_Live runs a real proxy, built from this checkout, on the engine
// at DOCKER_HOST, under the runtime serve builds for Docker (its decision sink on the relay address, the
// offline manager, and the organisation link, here a loopback hub that acknowledges every record):
//   - the proxy mounts nothing from the host, and its config names the sink;
//   - an agent request through it succeeds, and its decision lands in runnerd's spool and is
//     acknowledged by the organisation;
//   - a revive (ReplaceProxy) hands the run to a new proxy, whose request lands the same way;
//   - the containment stop (StopProxy) drains and settles it with nothing lost, and the records stay.
//
// The allowed host is an internal_hosts name the proxy's hosts map to a CGNAT bridge gateway where a
// host-network httpd answers, as the internal_hosts runner pin does. Skipped unless WARDYN_TEST_DOCKER=1.
func TestDockerProxySendsDecisionsToTheSink_Live(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker decision sink test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "wardyn-proxy")
	build := exec.Command("go", "build", "-o", binary, "../wardyn-proxy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wardyn-proxy: %v\n%s", err, out)
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	bind := liveSinkBind(t, ctx, cli)
	tag := uuid.NewString()[:8]
	internal := "wardyn-sink-live-" + tag
	sinkDocker(t, "network", "create", internal)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", internal).Run() })
	octet := strconv.Itoa(1 + mathrand.IntN(254))
	cgnatNet, cgnatIP := "wardyn-sink-cgnat-"+tag, "100.64."+octet+".1"
	sinkDocker(t, "network", "create", "--subnet", "100.64."+octet+".0/24", "--gateway", cgnatIP, cgnatNet)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", cgnatNet).Run() })
	port := strconv.Itoa(20000 + mathrand.IntN(10000))
	httpd := "wardyn-sink-httpd-" + tag
	sinkDocker(t, "run", "-d", "--name", httpd, "--network", "host", "busybox:latest",
		"sh", "-c", "mkdir -p /w && echo internal > /w/index.html && exec httpd -f -p "+port+" -h /w")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", httpd).Run() })
	const host = "svc.corp.internal"

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}
	stateDir := t.TempDir()
	sink, err := newDockerSink(ctx, stateDir, dockerEngine{})
	if err != nil {
		t.Fatal(err)
	}
	ev := &evidenceForwarder{}
	sub, err := docker.New(docker.Config{RunnerID: identity.RunnerID.String(), ProxyImage: "busybox:latest", ProxyBinaryHostPath: binary, InternalNetwork: internal,
		DecisionSink: sinkAdapter{s: sink.spoolSink, url: sink.proxyURL}, Evidence: ev, TestProxyExtraHosts: []string{host + ":" + cgnatIP}})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runnerRuntimeWith(ctx, identity, stateDir, sub, docker.SandboxRef, newLocalRelay(ctx), unsized, sink.extras(sub, ev))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.close() })
	if err := sink.use(bind); err != nil {
		t.Fatal(err)
	}

	// The organisation: a loopback hub that acknowledges every event, keeping the spool rows it got.
	const orgURL = "https://org.example"
	var orgMu sync.Mutex
	var orgRows []runnerspool.Record
	row := types.Runner{ID: identity.RunnerID, PublicKey: pub, State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerOrg, OrgURLSHA256: federation.OrgURLSHA256(orgURL)}
	generation, link := uuid.New(), &remote.Link{}
	org := remote.New(row.ID.String(), link, remote.Options{ExecutionGeneration: func(context.Context, string, string) (uuid.UUID, error) { return generation, nil }})
	hub := remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL,
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: org, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Activity:       func(context.Context, uuid.UUID) error { return nil },
				Event: func(_ context.Context, e runnerwire.Event) error {
					var r runnerspool.Record
					if e.Kind == runnerwire.EventSpool && json.Unmarshal(e.Data, &r) == nil {
						orgMu.Lock()
						orgRows = append(orgRows, r)
						orgMu.Unlock()
					}
					return nil
				}}, nil
		},
	})
	t.Cleanup(hub.Close)
	online := make(chan *runnerwire.Peer, 2)
	hooks := rt.hooks
	hooks.Online = func(p *runnerwire.Peer, resumed bool) { rt.hooks.Online(p, resumed); online <- p }
	wire, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			o, local := runnerwire.Loopback()
			go func() { slog.Info("sink live: hub session ended", "error", hub.Serve(ctx, o, row.ID)) }()
			return local, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	stopped := make(chan error, 2)
	go func() { stopped <- rt.run(runCtx) }()
	go func() { err := wire.Run(runCtx); slog.Info("sink live: wire ended", "error", err); stopped <- err }()
	t.Cleanup(func() { stop(); <-stopped; <-stopped })
	resumeAwait(t, ctx, online)
	for !link.Online() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	runID := uuid.New()
	lease, _ := runnerwire.Marshal(runnerwire.Lease{RunID: runID, ExecutionGeneration: generation, TokenRenewedAt: time.Now(), EndsAt: time.Now().Add(time.Hour)})
	if err := link.Send(ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: lease}); err != nil {
		t.Fatal(err)
	}
	pc := runner.ProxyConfig{RunToken: "sink-live-token", ControlPlaneURL: "http://127.0.0.1:9",
		Policy:        types.RunPolicySpec{AllowedDomains: []string{host}, MinConfinementClass: types.CC1},
		InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}
	created, err := org.CreateSandbox(ctx, runner.SandboxSpec{RunID: runID, RunnerID: row.ID.String(), Image: "busybox:latest", ConfinementClass: types.CC1, ProxyConfig: pc})
	if err != nil {
		t.Fatalf("CreateSandbox over the runner wire: %v", err)
	}
	ref := docker.SandboxRef(runID)
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		_ = sub.KillSandbox(clean, ref)
	})
	proxyName := "wardyn-proxy-" + runID.String()
	insp, err := cli.ContainerInspect(ctx, proxyName, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hc := insp.Container.HostConfig; len(hc.Mounts) != 0 || len(hc.Binds) != 1 || !strings.HasPrefix(hc.Binds[0], binary+":") {
		t.Fatalf("the proxy mounts %v %v; want only the test's proxy binary, and nothing of the state directory", hc.Mounts, hc.Binds)
	}
	spool, err := runnerspool.Open(filepath.Join(stateDir, "spools", runID.String()), runID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	fetch := func(step string) {
		t.Helper()
		code, out := sinkLiveExec(t, ctx, sub, ref, "http_proxy=http://wardyn-proxy:"+strconv.Itoa(runner.ProxyListenPort)+" wget -q -T 20 -O - http://"+host+":"+port+"/ 2>&1")
		if code != 0 || strings.TrimSpace(out) != "internal" {
			t.Fatalf("%s: the agent's request through its proxy = %d %q, want the page", step, code, out)
		}
	}
	// allowed is the organisation's rows that are allowed decisions for host.
	allowed := func() []runnerspool.Record {
		orgMu.Lock()
		defer orgMu.Unlock()
		var out []runnerspool.Record
		for _, r := range orgRows {
			if r.Decision.Request.Host == host && r.Decision.Decision == egress.Allow && r.ValidHash() {
				out = append(out, r)
			}
		}
		return out
	}
	// landed waits up to a minute until the organisation got the nth allowed decision for host from
	// runnerd's spool, and runnerd recorded its acknowledgement.
	landed := func(step string, n int) {
		t.Helper()
		ctx, done := context.WithTimeout(ctx, time.Minute)
		defer done()
		for len(allowed()) < n {
			sinkLiveWait(t, ctx, step+": the decision never reached the organisation through runnerd's spool")
		}
		seq := allowed()[n-1].Seq
		for {
			pos, err := spool.Get(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if pos.Seq >= seq {
				return
			}
			sinkLiveWait(t, ctx, step+": runnerd never recorded the organisation's acknowledgement")
		}
	}
	fetch("create")
	landed("create", 1)

	cfgJSON, err := runner.BuildProxyConfig(runID, pc, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := org.ReplaceProxy(ctx, created.Ref, cfgJSON); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	fetch("revive")
	landed("revive", 2)

	if err := org.StopProxy(ctx, created.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	l, err := spool.Liability(ctx)
	if err != nil || !l.Settled() || l.PendingLoss != nil {
		t.Fatalf("after the stop: liability %+v (%v); want the producer settled", l, err)
	}
	// A loss row, had the stop lost anything, was written before StopProxy returned: once the
	// organisation has every row, none may be one.
	for {
		depth, _, _, err := spool.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if depth == 0 {
			break
		}
		sinkLiveWait(t, ctx, "the organisation never got the spool's last rows")
	}
	orgMu.Lock()
	defer orgMu.Unlock()
	for _, r := range orgRows {
		if r.Decision.RuleSource == "builtin:spool-lost" {
			t.Fatalf("a loss row after drained stops: %+v", r)
		}
	}
	if len(orgRows) != 2 {
		t.Fatalf("the organisation got %d rows, want the two decisions", len(orgRows))
	}
}

// liveSinkBind is where a container on the engine reaches this host through host-gateway, as the relay
// live test finds it: the host's loopback on Docker Desktop, else the default bridge's gateway.
func liveSinkBind(t *testing.T, ctx context.Context, cli *client.Client) netip.Addr {
	t.Helper()
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Info.OperatingSystem == "Docker Desktop" {
		return netip.MustParseAddr("127.0.0.1")
	}
	insp, err := cli.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range insp.Network.IPAM.Config {
		if c.Gateway.Is4() {
			return c.Gateway
		}
	}
	t.Fatal("the default bridge has no IPv4 gateway")
	return netip.Addr{}
}

func sinkDocker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", args[0], err, out)
	}
}

// sinkLiveWait waits a moment before a poll is retried, failing with why once ctx ends.
func sinkLiveWait(t *testing.T, ctx context.Context, why string) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal(why)
	case <-time.After(50 * time.Millisecond):
	}
}

func sinkLiveExec(t *testing.T, ctx context.Context, sub *docker.Driver, ref, command string) (int, string) {
	t.Helper()
	session, err := sub.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", command}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, session.Stderr); close(drained) }()
	out, err := io.ReadAll(session.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	code, err := session.Wait()
	if err != nil {
		t.Fatal(err)
	}
	<-drained
	return code, string(out)
}
