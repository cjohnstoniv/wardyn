// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestLocalRuntimeRequiresLeaseAndPersistsCreation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}
	stateDir := t.TempDir()
	sub := runnertest.NewFake()
	ref := func(id uuid.UUID) string { return "fake-" + id.String() }
	runtime, err := localRunnerRuntime(ctx, identity, stateDir, sub, ref, newLocalRelay(ctx), unsized)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	const orgURL = "https://org.example"
	row := types.Runner{ID: identity.RunnerID, PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256(orgURL)}
	link := &remote.Link{}
	remoteSub := remote.New(row.ID.String(), link, remote.Options{})
	hub := remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL,
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: remoteSub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: runtime.hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, local := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return local, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var serviceErr error
	go func() { defer close(done); serviceErr = runRunnerServices(ctx, client.Run, runtime.run) }()
	defer func() { cancel(); <-done }()
	for !link.Online() {
		select {
		case <-done:
			t.Fatal("composed runtime stopped before READY", serviceErr)
		case <-ctx.Done():
			t.Fatal("composed runtime did not reach READY", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	run := uuid.New()
	spec := runner.SandboxSpec{RunID: run, RunnerID: row.ID.String(), ConfinementClass: types.CC1}
	if _, err := remoteSub.CreateSandbox(ctx, spec); err == nil {
		t.Fatal("production lifecycle hooks permitted create without an initial lease")
	}
	if _, err := sub.Status(ctx, ref(run)); !errors.Is(err, runner.ErrSandboxGone) {
		t.Fatalf("missing-lease create reached substrate: %v", err)
	}
	lease, _ := runnerwire.Marshal(runnerwire.Lease{RunID: run, TokenRenewedAt: time.Now(), EndsAt: time.Now().Add(time.Hour)})
	if err := link.Send(ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: lease}); err != nil {
		t.Fatal(err)
	}
	created, err := remoteSub.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remoteSub.CreateSandbox(ctx, spec); err == nil {
		t.Fatal("production lifecycle hooks permitted ambiguous create retry")
	}
	exec, err := remoteSub.ExecStream(ctx, created.Ref, runner.ExecSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatalf("exec into the created sandbox: %v", err)
	}
	_ = exec.Close()
	unheld, err := sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if exec, err := remoteSub.ExecStream(ctx, placement.RunnerRefPrefix(row.ID.String())+unheld.Ref, runner.ExecSpec{Argv: []string{"cat"}}); err == nil {
		_ = exec.Close()
		t.Fatal("exec reached a sandbox the runner did not create")
	}
	cancel()
	// Both services stop before the lifecycle lock is released for restart.
	<-done
	if err := runtime.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := localRunnerRuntime(context.Background(), identity, stateDir, sub, ref, newLocalRelay(context.Background()), unsized)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.close()
	if !sub.Called("stop_proxy", ref(run)) {
		t.Fatal("daemon restart did not contain the durably bound proxy before connecting")
	}
	// The inventory is emitted only while execution admission is closed.
	var state []runnerwire.RunState
	err = restarted.hooks.StatePages(context.Background(), func(page runnerwire.StatePage) error {
		state = append(state, page.Runs...)
		return nil
	})
	if err != nil || len(state) != 1 || state[0].RunID != run || !state[0].SandboxPresent {
		t.Fatalf("actual durable lifecycle state missing: %#v %v", state, err)
	}
}

// TestRelay_RunnerNeverOpensTowardTheOrg: a relay is opened by the runner,
// outbound, and never accepted the other way. The production hooks refuse
// every stream the org opens, a relay included, so the org cannot make a
// runner dial anything.
func TestRelay_RunnerNeverOpensTowardTheOrg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := localRunnerRuntime(ctx, runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key}, t.TempDir(), runnertest.NewFake(),
		func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx), unsized)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	for _, kind := range []string{runnerwire.KindRelay, runnerwire.KindPTY, runnerwire.KindExec, runnerwire.KindOutput} {
		if err := runtime.hooks.Open(nil, nil, runnerwire.Open{Kind: kind}); err == nil {
			t.Errorf("the runner accepted an org-opened %s stream", kind)
		}
	}
}

// unsized is a runtime's capacity read for a runner with no engine to ask: unreported.
func unsized(context.Context) placement.Capacity { return placement.Capacity{} }

// fakeEngine answers the Docker API serve reads at startup: the engine's
// operating system and name, its default bridge (whose interface, iface, may
// or may not be on this host), and whether the named network and the proxy
// image exist.
type fakeEngine struct {
	os, name   string
	arch       string // the engine's reported Architecture; "" omits it
	iface      string // the default bridge's interface; "" is one no host has
	network    bool
	createCode int
	created    []string // bodies of each network create
	image      bool     // the proxy image is on the engine
	pullCode   int      // what a pull answers; 0 pulls the image
}

func (f *fakeEngine) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := regexp.MustCompile(`^/v[0-9.]+`).ReplaceAllString(r.URL.Path, "")
		switch {
		case path == "/_ping":
			w.Header().Set("Api-Version", "1.44")
		case path == "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"OperatingSystem": f.os, "Name": f.name, "Architecture": f.arch})
		case path == "/networks/bridge":
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "bridge", "Id": "b", "Driver": "bridge",
				"IPAM":    map[string]any{"Config": []map[string]string{{"Subnet": "172.17.0.0/16", "Gateway": "172.17.0.1"}}},
				"Options": map[string]string{"com.docker.network.bridge.name": cmp.Or(f.iface, "wardyn-test-nobr")}})
		case path == "/networks/create" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			f.created = append(f.created, string(body))
			w.WriteHeader(cmp.Or(f.createCode, http.StatusCreated))
			_, _ = io.WriteString(w, `{"Id":"n1","message":"create"}`)
		case path == "/networks/wardyn-internal" && f.network:
			_, _ = io.WriteString(w, `{"Name":"wardyn-internal","Id":"n1","Driver":"bridge"}`)
		case path == "/images/json":
			if f.image {
				_, _ = io.WriteString(w, `[{"Id":"sha256:1","RepoTags":["wardyn-proxy:test"]}]`)
			} else {
				_, _ = io.WriteString(w, `[]`)
			}
		case path == "/images/create":
			if f.pullCode != 0 {
				w.WriteHeader(f.pullCode)
				_, _ = io.WriteString(w, `{"message":"pull access denied"}`)
				return
			}
			f.image = true
			_, _ = io.WriteString(w, `{"status":"pulled"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"no such object"}`)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
}

// rancherLima answers as Rancher Desktop's macOS VM did in a reported docker
// info (RD 1.19.3): its bridge lives in the VM, not on this host.
func rancherLima() *fakeEngine {
	return &fakeEngine{os: "Alpine Linux v3.21", name: "lima-rancher-desktop", network: true, image: true}
}

// rancherLimaV324Arm64 is the same VM on an Apple-silicon Mac with the newer
// Alpine base: a different OperatingSystem string, still not Docker Desktop.
func rancherLimaV324Arm64() *fakeEngine {
	return &fakeEngine{os: "Alpine Linux v3.24", name: "lima-rancher-desktop", arch: "aarch64", network: true, image: true}
}

// serveWith runs productionRunnerHooks against engine.
func serveWith(t *testing.T, engine *fakeEngine, relayListen, hostTarget string) error {
	t.Helper()
	if engine == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DOCKER_HOST", "tcp://"+ln.Addr().String())
		_ = ln.Close()
	} else {
		engine.start(t)
	}
	opts := serveOptions{proxyImage: "wardyn-proxy:test", relayListen: relayListen, relayHostTarget: hostTarget, network: "wardyn-internal", recording: "off"}
	runtime, err := productionRunnerHooks(context.Background(), runneridentity.Identity{RunnerID: uuid.New()}, t.TempDir(), opts)
	if runtime.close != nil {
		_ = runtime.close()
	}
	return err
}

// A runner whose relay or Docker engine cannot serve runs refuses to start,
// so it never shows online and then refuses every run at create.
func TestServeFailsFastWhenRunsCannotStart(t *testing.T) {
	for _, tc := range []struct {
		name, relayListen, hostTarget, want string
		engine                              *fakeEngine // nil: nothing answers at DOCKER_HOST
	}{
		{"engine down", "", "", "docker engine not reachable", nil},
		{"every interface", "0.0.0.0", "", "refusing to listen on 0.0.0.0", &fakeEngine{os: "Docker Desktop"}},
		{"not an address", "localhost", "", "--relay-listen", &fakeEngine{os: "Docker Desktop"}},
		{"network create refused", "127.0.0.1", "", "create docker network wardyn-internal", &fakeEngine{os: "Docker Desktop", createCode: http.StatusForbidden}},
		{"host target not an address", "", "localhost", "--relay-host-target", &fakeEngine{os: "Docker Desktop"}},
		{"host target loopback", "", "127.0.0.1", "--relay-host-target", &fakeEngine{os: "Docker Desktop"}},
		{"image absent, pull refused", "", "", "the proxy image wardyn-proxy:test is not on this engine and could not be pulled (a registry that needs a login: docker pull",
			&fakeEngine{os: "Docker Desktop", network: true, pullCode: http.StatusForbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := serveWith(t, tc.engine, tc.relayListen, tc.hostTarget)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("productionRunnerHooks = %v, want an error naming %q", err, tc.want)
			}
			if tc.engine != nil && strings.Contains(tc.want, "relay") && len(tc.engine.created) != 0 {
				t.Errorf("a refused relay flag still created a network: %q", tc.engine.created)
			}
		})
	}
}

// inspectEngine tells native from VM-backed before any relay address is
// judged: a bridge not on this host is a fact, never an error, and Docker
// Desktop is told by its OperatingSystem without the bridge test.
func TestInspectEngine(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	for _, tc := range []struct {
		name   string
		engine *fakeEngine
		native bool
		why    string
	}{
		{"native", &fakeEngine{os: "Ubuntu 24.04 LTS", name: "laptop", iface: "lo"}, true, ""},
		{"Docker Desktop beside a host bridge", &fakeEngine{os: "Docker Desktop", name: "docker-desktop", iface: "lo"}, false, "Docker Desktop"},
		{"VM-backed", rancherLima(), false, "its bridge interface is not on this host"},
		{"VM-backed, Alpine v3.24 on arm64", rancherLimaV324Arm64(), false, "its bridge interface is not on this host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.engine.start(t)
			cli, err := client.New(client.FromEnv)
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			eng, err := inspectEngine(context.Background(), cli, "unreachable")
			if err != nil {
				t.Fatalf("inspectEngine: %v", err)
			}
			if eng.native != tc.native || !strings.HasPrefix(eng.why, tc.why) || eng.native != (eng.gateway == gw) || eng.native != slices.Contains(eng.bridge, relayLoopback) {
				t.Fatalf("engine = %+v, want native=%v why %q", eng, tc.native, tc.why)
			}
		})
	}
}

// readyEngine creates a missing proxy-sidecar bridge as the installer does,
// leaves an existing one alone, and creates nothing for a refused bind.
func TestReadyEngineCreatesTheMissingNetwork(t *testing.T) {
	ctx := context.Background()
	ready := func(t *testing.T, f *fakeEngine, bind string) error {
		t.Helper()
		f.start(t)
		cli, err := client.New(client.FromEnv)
		if err != nil {
			t.Fatal(err)
		}
		defer cli.Close()
		_, err = readyEngine(ctx, cli, "unreachable", "wardyn-internal", relayPair{bind: netip.MustParseAddr(bind)})
		return err
	}
	missing := &fakeEngine{os: "Docker Desktop"}
	if err := ready(t, missing, "127.0.0.1"); err != nil || len(missing.created) != 1 || !strings.Contains(missing.created[0], `"Name":"wardyn-internal"`) || !strings.Contains(missing.created[0], `"Driver":"bridge"`) {
		t.Fatalf("err=%v creates=%q, want one bridge named wardyn-internal", err, missing.created)
	}
	present := &fakeEngine{os: "Docker Desktop", network: true}
	if err := ready(t, present, "127.0.0.1"); err != nil || len(present.created) != 0 {
		t.Fatalf("existing network: err=%v creates=%q", err, present.created)
	}
	// The reported case: --relay-listen 127.0.0.1 on an engine whose bridge is not on this host.
	if err := ready(t, rancherLima(), "127.0.0.1"); err != nil {
		t.Fatalf("an explicit loopback on a VM-backed engine: %v", err)
	}
	refused := &fakeEngine{os: "Docker Desktop"}
	if err := ready(t, refused, "192.168.1.20"); err == nil || len(refused.created) != 0 {
		t.Fatalf("a refused bind still touched the engine's networks: err=%v creates=%q", err, refused.created)
	}
}
