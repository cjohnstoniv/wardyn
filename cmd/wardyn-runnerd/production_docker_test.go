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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	runtime, err := localRunnerRuntime(ctx, identity, stateDir, sub, ref, newLocalRelay(ctx, netip.Addr{}), unsized)
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
	restarted, err := localRunnerRuntime(context.Background(), identity, stateDir, sub, ref, newLocalRelay(context.Background(), netip.Addr{}), unsized)
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
		func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx, netip.Addr{}), unsized)
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
// operating system and whether the named network exists.
type fakeEngine struct {
	os         string
	network    bool
	createCode int
	created    []string // bodies of each network create
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
			_ = json.NewEncoder(w).Encode(map[string]any{"OperatingSystem": f.os})
		case path == "/networks/create" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			f.created = append(f.created, string(body))
			w.WriteHeader(cmp.Or(f.createCode, http.StatusCreated))
			_, _ = io.WriteString(w, `{"Id":"n1","message":"create"}`)
		case path == "/networks/wardyn-internal" && f.network:
			_, _ = io.WriteString(w, `{"Name":"wardyn-internal","Id":"n1","Driver":"bridge"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"no such object"}`)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
}

// A runner whose relay or Docker engine cannot serve runs refuses to start,
// so it never shows online and then refuses every run at create.
func TestServeFailsFastWhenRunsCannotStart(t *testing.T) {
	for _, tc := range []struct {
		name, relayListen, want string
		engine                  *fakeEngine // nil: nothing answers at DOCKER_HOST
	}{
		{"engine down", "", "docker engine not reachable", nil},
		{"Docker Desktop without --relay-listen", "", "--relay-listen 127.0.0.1", &fakeEngine{os: "Docker Desktop"}},
		{"Docker Desktop on every interface", "0.0.0.0", "refusing --relay-listen", &fakeEngine{os: "Docker Desktop"}},
		{"not an address", "localhost", "--relay-listen", &fakeEngine{os: "Docker Desktop"}},
		{"network create refused", "127.0.0.1", "create docker network wardyn-internal", &fakeEngine{os: "Docker Desktop", createCode: http.StatusForbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.engine == nil {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("DOCKER_HOST", "tcp://"+ln.Addr().String())
				_ = ln.Close()
			} else {
				tc.engine.start(t)
			}
			opts := serveOptions{proxyImage: "wardyn-proxy:test", relayListen: tc.relayListen, network: "wardyn-internal", recording: "off"}
			runtime, err := productionRunnerHooks(context.Background(), runneridentity.Identity{RunnerID: uuid.New()}, t.TempDir(), opts)
			if runtime.close != nil {
				_ = runtime.close()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("productionRunnerHooks = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// serve creates a missing proxy-sidecar bridge as the installer does, and
// leaves an existing one alone.
func TestPrepareDockerCreatesTheMissingNetwork(t *testing.T) {
	ctx := context.Background()
	loopback := netip.MustParseAddr("127.0.0.1")
	missing := &fakeEngine{os: "Docker Desktop"}
	missing.start(t)
	addr, err := prepareDocker(ctx, "wardyn-internal", loopback)
	if err != nil || addr != loopback {
		t.Fatalf("prepareDocker = %s, %v", addr, err)
	}
	if len(missing.created) != 1 || !strings.Contains(missing.created[0], `"Name":"wardyn-internal"`) || !strings.Contains(missing.created[0], `"Driver":"bridge"`) {
		t.Fatalf("network creates = %q, want one bridge named wardyn-internal", missing.created)
	}
	present := &fakeEngine{os: "Docker Desktop", network: true}
	present.start(t)
	if _, err := prepareDocker(ctx, "wardyn-internal", loopback); err != nil || len(present.created) != 0 {
		t.Fatalf("existing network: err=%v creates=%q", err, present.created)
	}
	desktop := &fakeEngine{os: "Docker Desktop"}
	desktop.start(t)
	if _, err := prepareDocker(ctx, "wardyn-internal", netip.Addr{}); err == nil || len(desktop.created) != 0 {
		t.Fatalf("an unusable relay still touched the engine's networks: err=%v creates=%q", err, desktop.created)
	}
}
