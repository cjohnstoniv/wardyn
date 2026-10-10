// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/runneragent"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

func TestConformanceDockerThroughRemote(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping live remote Docker conformance")
	}
	for _, wire := range []string{"loopback", "websocket"} {
		t.Run(wire, func(t *testing.T) {
			roots, drives := dockerDriveFixture(t)
			sub, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"sleep", "infinity"},
				InternalNetwork: remoteConformanceNetwork(t), UserDriveHostRoots: roots})
			if err != nil {
				t.Fatal(err)
			}
			r := remoteDockerRunner(t, sub, wire == "websocket")
			requireStrongestClass(t, r)
			conformance.Run(t, r, conformance.Options{
				SandboxImage: "busybox:latest", Timeout: 3 * time.Minute,
				DefaultRouteProbe: func(t *testing.T, ref string) {
					local, ok := strings.CutPrefix(ref, placement.RunnerRefPrefix(r.id))
					if !ok || local == "" {
						t.Fatal("probe did not receive this runner's sandbox ref")
					}
					dockerRouteProbe(t, local)
				},
				ExitArgv:       func(code int) []string { return []string{"sh", "-c", "exit " + strconv.Itoa(code)} },
				AgentUserImage: agentUserImage(t, "busybox:latest"), UserDrives: drives, ExecState: dockerExecState,
			})
		})
	}
}

func remoteConformanceNetwork(t *testing.T) string {
	t.Helper()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	name := "wardyn-conformance-remote-" + uuid.NewString()
	created, err := cli.NetworkCreate(context.Background(), name, dockerclient.NetworkCreateOptions{Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = cli.NetworkRemove(ctx, created.ID, dockerclient.NetworkRemoveOptions{})
	})
	return name
}

// The production orchestrator deliberately excludes remote runners from the
// deployment's capabilities. This view binds the unchanged suite to one runner
// while still exercising actual remote capabilities, dispatch and ref routing.
type boundRemoteRunner struct {
	*orchestrator.Orchestrator
	id   string
	caps *orchestrator.Orchestrator
}

func (r *boundRemoteRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	caps, err := r.caps.Capabilities(ctx)
	caps.Driver = r.Name()
	return caps, err
}

func (r *boundRemoteRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	spec.RunnerID = r.id
	return r.Orchestrator.CreateSandbox(ctx, spec)
}

func (r *boundRemoteRunner) KillSandbox(ctx context.Context, ref string) error {
	// The suite's missing-ref probe is intentionally unscoped. Qualify it in
	// this bound view; production continues to refuse foreign references.
	if !strings.HasPrefix(ref, substrate.RemotePrefix) {
		ref = placement.RunnerRefPrefix(r.id) + ref
	}
	return r.Orchestrator.KillSandbox(ctx, ref)
}

type remoteCapabilityView struct{ substrate.Substrate }

func (remoteCapabilityView) Name() string { return "bound-runner-capabilities" }

func remoteDockerRunner(t *testing.T, local substrate.Substrate, realWire bool) *boundRemoteRunner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed}
	link := &remote.Link{}
	generation := uuid.New()
	sub := remote.New(row.ID.String(), link, remote.Options{
		ExecutionGeneration: func(context.Context, string, string) (uuid.UUID, error) { return generation, nil },
	})
	streams, err := runnerio.New(local, row.ID.String(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := runneragent.New(runneragent.Config{RunnerID: row.ID.String(), Substrate: local,
		BeforeCreate:  func(context.Context, runner.SandboxSpec) error { return nil },
		Created:       func(context.Context, runner.SandboxSpec, string) error { return nil },
		CreateFailed:  func(context.Context, runner.SandboxSpec) error { return nil },
		BeforeRefCall: func(context.Context, string, string, uuid.UUID) error { return nil },
		AfterRefCall:  func(context.Context, string, string, string, uuid.UUID) error { return nil },
		Streams:       streams.Call, Output: streams.Output, BindOutput: streams.BindOutput, ForgetOutput: streams.ForgetOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	orgURL := "https://org.example"
	var hub *remote.Hub
	if realWire {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = hub.Serve(r.Context(), runnerwire.NewWebSocket(conn), row.ID)
			}
		}))
		t.Cleanup(server.Close)
		orgURL = server.URL
	}
	row.OrgURLSHA256 = federation.OrgURLSHA256(orgURL)
	hub = remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL,
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: sub, Link: link,
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				State:   func(context.Context, []runnerwire.RunState) error { return nil },
				Event:   func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	t.Cleanup(hub.Close)
	// H3 routing and H6 offline/spool policy are explicit test doubles here.
	// This suite proves the real transport, dispatcher, Docker and H4 adapters.
	hooks := runnerwire.RunnerHooks{
		Call: dispatch.Call, Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return runnerwire.ErrNotReady },
		Clock:       func(runnerwire.Hello, time.Time) error { return nil },
		BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
		Pending: func(_ context.Context, _ *runnerwire.Peer, actions []runnerwire.PendingAction) error {
			if len(actions) != 0 {
				return errors.New("conformance fixture has no durable pending actions")
			}
			return nil
		},
		State: func(context.Context) ([]runnerwire.RunState, error) { return nil, nil },
		Replay: func(ctx context.Context, peer *runnerwire.Peer) error {
			cs, err := local.Classes(ctx)
			if err != nil {
				return err
			}
			data, err := runnerwire.Marshal(runnerwire.Caps{Support: cs, Version: "conformance"})
			if err != nil {
				return err
			}
			return peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
		},
		Lease:   func(context.Context, runnerwire.Lease) error { return errors.New("unexpected conformance lease") },
		Revoked: func(context.Context) error { return nil }, Offline: func(context.Context) {},
		Online: func(p *runnerwire.Peer, _ bool) { streams.Online(p) }, StreamOffline: streams.Offline,
	}
	cfg := runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks}
	if !realWire {
		cfg.Dial = func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return run, nil
		}
	}
	client, err := runnerwire.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Connect(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	wait, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	for !link.Online() {
		select {
		case <-wait.Done():
			t.Fatal("authenticated remote runner did not reach READY:", wait.Err())
		case err := <-done:
			done <- err
			t.Fatal("remote connection ended before READY:", err)
		case <-time.After(time.Millisecond):
		}
	}
	return &boundRemoteRunner{Orchestrator: orchestrator.New(sub), id: row.ID.String(), caps: orchestrator.New(remoteCapabilityView{sub})}
}
