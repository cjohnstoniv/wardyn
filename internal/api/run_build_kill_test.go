// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stoppableBuilder is an image builder whose builds run until their context
// ends, as a pull or a build that honours cancellation does. A closed release
// ends them built instead. stopErr is what stopping cost the builder.
type stoppableBuilder struct {
	entered chan struct{} // one send per build that started
	release chan struct{}
	stopErr error
	calls   atomic.Int32
}

func newStoppableBuilder() *stoppableBuilder {
	return &stoppableBuilder{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (b *stoppableBuilder) build(ctx context.Context, tag string) (BuiltImage, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return BuiltImage{Ref: tag, ID: "sha256:built"}, nil
	case <-ctx.Done():
		return BuiltImage{}, errors.Join(fmt.Errorf("envbuild: build cancelled or timed out: %w", ctx.Err()), b.stopErr)
	}
}

func (b *stoppableBuilder) FinalizeBase(ctx context.Context, _, tag string, _ io.Writer, _ bool) (BuiltImage, error) {
	return b.build(ctx, tag)
}

func (b *stoppableBuilder) BuildDevcontainer(ctx context.Context, _, _, tag string, _ io.Writer) (BuiltImage, error) {
	return b.build(ctx, tag)
}

func (b *stoppableBuilder) BuildFromDevcontainerFiles(ctx context.Context, _ map[string]string, tag string, _ io.Writer) (BuiltImage, error) {
	return b.build(ctx, tag)
}

// liveCtxAudit counts the rows written on a context that had already ended.
type liveCtxAudit struct {
	recRecorder
	dead atomic.Int32
}

func (a *liveCtxAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if ctx.Err() != nil {
		a.dead.Add(1)
	}
	return a.recRecorder.Record(ctx, ev)
}

// runBuildDoors are the four places a run's image is built: the three arms of
// create (a wrapped base image, a devcontainer repo, an onboarded workspace) and
// a workspace run's launch.
var runBuildDoors = []struct {
	name  string
	build func(*Server, context.Context, uuid.UUID)
}{
	{"create wraps a base image", func(s *Server, ctx context.Context, id uuid.UUID) {
		s.resolveCreateRunImage(ctx, createRunRequest{Agent: "claude-code", Image: "ubuntu:24.04"}, id, nil)
	}},
	{"create builds a devcontainer repo", func(s *Server, ctx context.Context, id uuid.UUID) {
		s.resolveCreateRunImage(ctx, createRunRequest{Agent: "claude-code", DevcontainerRepo: "https://github.com/acme/widgets"}, id, nil)
	}},
	{"create builds a workspace's image", func(s *Server, ctx context.Context, id uuid.UUID) {
		s.resolveCreateRunImage(ctx, createRunRequest{Agent: "claude-code"}, id, []types.Workspace{buildDoorWorkspace()})
	}},
	{"a workspace run builds its image", func(s *Server, ctx context.Context, id uuid.UUID) {
		s.workspaceRunImage(ctx, id, buildDoorWorkspace())
	}},
}

func buildDoorWorkspace() types.Workspace {
	return types.Workspace{ID: uuid.New(), BaseImage: &types.WorkspaceBaseImage{Kind: "registry", Image: "ubuntu:24.04"}}
}

// runBuildFixture is a server with no runner and one PENDING run, on a context
// that ends with the test so a build nothing stopped does not outlive it.
func runBuildFixture(t *testing.T, b *stoppableBuilder) (*Server, *createDetachStore, *liveCtxAudit, types.AgentRun, context.Context) {
	t.Helper()
	st := newCreateDetachStore()
	audit := &liveCtxAudit{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Audit, cfg.ImageBuilder = audit, b
	run := newFinalizeRun()
	run.State = types.RunPending
	if _, err := st.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(cfg), st, audit, run, ctx
}

// killClaim is the first half of a kill: the KILLED write, then the notice that
// stops what is starting the run.
func killClaim(t *testing.T, srv *Server, run types.AgentRun) {
	t.Helper()
	if ok, err := srv.claimKillTransition(context.Background(), run); err != nil || !ok {
		t.Fatalf("kill claim = %v, %v", ok, err)
	}
}

func waitBuild(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestRunBuild_AKillStopsTheBuild: a kill of a run whose image is building ends
// the build's context at each of the four doors, the run stays KILLED, and the
// build's failure row is written on a context the kill did not end. The row
// carries whatever the builder reported about stopping; nothing says the host
// is clean.
func TestRunBuild_AKillStopsTheBuild(t *testing.T) {
	uncleared := errors.New("envbuild: remove the build container: daemon unreachable")
	for _, door := range runBuildDoors {
		for _, stopErr := range []error{nil, uncleared} {
			name := door.name
			if stopErr != nil {
				name += ", and the builder could not clean up"
			}
			t.Run(name, func(t *testing.T) {
				b := newStoppableBuilder()
				b.stopErr = stopErr
				srv, st, audit, run, ctx := runBuildFixture(t, b)
				done := make(chan struct{})
				go func() {
					defer close(done)
					door.build(srv, ctx, run.ID)
				}()
				waitBuild(t, b.entered, "the build to start")

				killClaim(t, srv, run)
				waitBuild(t, done, "the build to end with its run's kill")

				if got := st.state(run.ID); got != types.RunKilled {
					t.Errorf("state = %s, want the kill's KILLED left alone", got)
				}
				row := findAudit(audit.snapshot(), run.ID, "run.build", "failure")
				if row == nil || !strings.Contains(string(row.Data), "context canceled") {
					t.Fatalf("run.build failure row = %v, want one carrying the cancelled build", row)
				}
				if stopErr != nil && !strings.Contains(string(row.Data), "daemon unreachable") {
					t.Errorf("run.build failure row = %s, want what the builder could not clean up", row.Data)
				}
				if n := audit.dead.Load(); n != 0 {
					t.Errorf("%d audit rows were written on a context that had ended", n)
				}
			})
		}
	}
}

// TestRunBuild_AKilledRunBuildsNothing: a kill that landed before the build
// registered found nothing to cancel, so the run is read again after the
// registration and a terminal one starts no build.
func TestRunBuild_AKilledRunBuildsNothing(t *testing.T) {
	for _, door := range runBuildDoors {
		t.Run(door.name, func(t *testing.T) {
			b := newStoppableBuilder()
			close(b.release)
			srv, st, _, run, ctx := runBuildFixture(t, b)
			killClaim(t, srv, run)

			door.build(srv, ctx, run.ID)

			if n := b.calls.Load(); n != 0 {
				t.Errorf("builds started for a killed run = %d, want 0", n)
			}
			if got := st.state(run.ID); got != types.RunKilled {
				t.Errorf("state = %s, want KILLED", got)
			}
		})
	}
}

// launchStore is dispatchTestStore with the image write a launch makes.
type launchStore struct{ *dispatchTestStore }

func (launchStore) SetRunImage(context.Context, uuid.UUID, string) error { return nil }

// heldCreateRunner holds CreateSandbox until its context ends.
type heldCreateRunner struct {
	*fakeRunner
	entered chan struct{}
}

func (r *heldCreateRunner) CreateSandbox(ctx context.Context, _ runner.SandboxSpec) (runner.Sandbox, error) {
	r.entered <- struct{}{}
	<-ctx.Done()
	return runner.Sandbox{}, ctx.Err()
}

// TestRunBuild_KeepsTheQueueAndDispatchRegistrations: a run started from a pool
// queue is registered for its kill three times in turn, by the queue start, the
// build and the dispatch. The build's registration is its own: a kill during the
// build reaches the queue start's too, a finished build leaves the queue start's
// in place, and a kill during the create that follows still stops that create.
func TestRunBuild_KeepsTheQueueAndDispatchRegistrations(t *testing.T) {
	launch := func(t *testing.T, withRunner bool) (*Server, *dispatchTestStore, *stoppableBuilder, *heldCreateRunner, context.Context, func(), <-chan struct{}) {
		t.Helper()
		rn := &heldCreateRunner{fakeRunner: &fakeRunner{}, entered: make(chan struct{}, 1)}
		srv, st, _, run := dispatchTeardownFixture(t, rn, types.RunPending)
		srv.cfg.Store = launchStore{st}
		if !withRunner {
			srv.cfg.Runner = nil
		}
		b := newStoppableBuilder()
		srv.cfg.ImageBuilder = b
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		// What startQueuedRun holds across the launch.
		queued, untrack := srv.creates.track(ctx, run.ID)
		t.Cleanup(untrack)
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.finishCreateRunLaunch(ctx, createRunLaunch{
				req:     createRunRequest{Agent: "claude-code", Image: "ubuntu:24.04"},
				spec:    types.RunPolicySpec{MinConfinementClass: types.CC1},
				ceiling: ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()),
				created: run, runToken: "run-token",
			})
		}()
		waitBuild(t, b.entered, "the build to start")
		kill := func() {
			st.mu.Lock()
			st.state = types.RunKilled
			st.mu.Unlock()
			srv.cancelCreate(run.ID)
		}
		return srv, st, b, rn, queued, kill, done
	}

	t.Run("a kill during the build reaches the queue start as well", func(t *testing.T) {
		_, st, _, rn, queued, kill, done := launch(t, true)
		kill()
		waitBuild(t, done, "the launch to end with its run's kill")
		if queued.Err() == nil {
			t.Error("the queue start's registration was not cancelled")
		}
		if len(rn.entered) != 0 || st.State() != types.RunKilled {
			t.Errorf("creates = %d, state = %s; want no sandbox and the run KILLED", len(rn.entered), st.State())
		}
	})

	t.Run("a finished build leaves the queue start's registration in place", func(t *testing.T) {
		_, _, b, _, queued, kill, done := launch(t, false)
		close(b.release)
		waitBuild(t, done, "the launch to end")
		if queued.Err() != nil {
			t.Fatal("the build ended the queue start's context")
		}
		kill()
		if queued.Err() == nil {
			t.Error("the build removed the queue start's registration: a later kill no longer reaches it")
		}
	})

	t.Run("a kill during the create after the build stops the create", func(t *testing.T) {
		_, st, b, rn, _, kill, done := launch(t, true)
		close(b.release)
		waitBuild(t, rn.entered, "CreateSandbox to start")
		kill()
		waitBuild(t, done, "the launch to end with its run's kill")
		if got := st.State(); got != types.RunKilled {
			t.Errorf("state = %s, want KILLED", got)
		}
	})
}
