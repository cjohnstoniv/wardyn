// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerSub is a fake remote substrate: its Name is "runner:<id>" and its refs
// carry the "runner:<id>/" prefix.
func runnerSub(id string) *fakeSubstrate {
	return &fakeSubstrate{name: substrate.RemotePrefix + id, classes: []types.ConfinementClass{types.CC1, types.CC3}, refPrefix: substrate.RemotePrefix + id + "/", drives: true, managed: true}
}

func TestOrchestrator_RunnerIDRoutesToThatRunner(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	a, b := runnerSub("a"), runnerSub("b")
	o := New(docker)
	o.Add(a)
	o.Add(b)

	spec := specFor(types.CC1)
	spec.RunnerID = "b"
	sb, err := o.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if len(b.created) != 1 || len(a.created) != 0 || len(docker.created) != 0 {
		t.Fatalf("create went to docker=%d a=%d b=%d, want only runner b", len(docker.created), len(a.created), len(b.created))
	}
	if _, err := o.Status(context.Background(), sb.Ref); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(b.statuses) != 1 || len(a.statuses) != 0 || len(docker.statuses) != 0 {
		t.Fatalf("status routed docker=%v a=%v b=%v; the ref must follow its runner", docker.statuses, a.statuses, b.statuses)
	}
	// Empty RunnerID is the cluster, unchanged.
	if _, err := o.CreateSandbox(context.Background(), specFor(types.CC1)); err != nil || len(docker.created) != 1 {
		t.Fatalf("empty RunnerID: err=%v docker.created=%d", err, len(docker.created))
	}
}

// An unregistered runner is offline, and the create is never substituted onto
// the cluster even though the cluster could run the spec.
func TestOrchestrator_UnknownRunnerIsOfflineNeverSubstituted(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	o := New(docker)
	spec := specFor(types.CC1)
	spec.RunnerID = uuid.NewString()
	_, err := o.CreateSandbox(context.Background(), spec)
	if !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("create for an unregistered runner = %v, want ErrRunnerOffline", err)
	}
	if len(docker.created) != 0 {
		t.Fatal("a local placement was substituted onto the cluster")
	}
}

func TestOrchestrator_RemoveStopsRoutingToTheRunner(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	r := runnerSub("r1")
	o := New(docker)
	o.Add(r)
	spec := specFor(types.CC1)
	spec.RunnerID = "r1"
	sb, err := o.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	o.Remove(r)
	if _, err := o.CreateSandbox(context.Background(), spec); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("create after Remove = %v, want ErrRunnerOffline", err)
	}
	if _, err := o.Status(context.Background(), sb.Ref); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("a ref of a removed runner = %v, want ErrRunnerOffline", err)
	}
	if len(docker.statuses) != 0 {
		t.Fatal("a removed runner's ref fell back to the cluster substrate")
	}
	o.Remove(r) // removing twice is harmless
}

// A ref carrying "runner:<id>/" resolves by the runner it names after a
// restart, when byRef is empty, and never takes the single-substrate fallback.
func TestOrchestrator_RunnerRefsRehydrateByPrefix(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	r := runnerSub("r2")
	o := New(docker)
	ref := "runner:r2/abc"
	if _, err := o.Status(context.Background(), ref); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("a runner ref before its substrate is added = %v, want ErrRunnerOffline", err)
	}
	if len(docker.statuses) != 0 {
		t.Fatal("a runner ref took the single-substrate fallback")
	}
	o.Add(r)
	if _, err := o.Status(context.Background(), ref); err != nil || len(r.statuses) != 1 || r.statuses[0] != ref {
		t.Fatalf("Status after Add: err=%v statuses=%v", err, r.statuses)
	}
	// A cluster ref still takes the fallback with a runner registered.
	if _, err := o.Status(context.Background(), "plain"); err != nil || len(docker.statuses) != 1 {
		t.Fatalf("cluster ref: err=%v docker.statuses=%v", err, docker.statuses)
	}
}

func TestOrchestrator_RemoteSubstratesAreInvisibleToCapabilitiesAndName(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}, structural: true}
	r := runnerSub("r3")
	r.structural = true
	o := New(docker)
	before, err := o.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o.Add(r)
	after, err := o.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ConfinementClasses) != len(before.ConfinementClasses) || after.Driver != "docker" {
		t.Fatalf("a registered runner changed the deployment's capabilities: %+v -> %+v", before, after)
	}
	for _, c := range after.ConfinementClasses {
		if c == types.CC3 {
			t.Fatal("the runner's CC3 leaked into the deployment capabilities")
		}
	}
	if r.classesCalls.Load() != 0 {
		t.Fatal("Capabilities probed a remote substrate")
	}
	// Class routing never picks a runner either.
	if _, err := o.CreateSandbox(context.Background(), specFor(types.CC3)); err == nil {
		t.Fatal("a class only a runner offers was routed to it without a RunnerID")
	}
}

func TestOrchestrator_SweepSkipsRemoteSubstrates(t *testing.T) {
	r := &sweepRemote{fakeSubstrate: runnerSub("r4")}
	o := New(&fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}})
	o.Add(r)
	if _, err := o.SweepOrphanedSandboxes(context.Background(), time.Minute, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if r.swept {
		t.Fatal("the orchestrator's crash sweep reached a remote substrate")
	}
}

type sweepRemote struct {
	*fakeSubstrate
	swept bool
}

func (s *sweepRemote) SweepOrphanedSandboxes(context.Context, time.Duration, func(uuid.UUID) bool) (int, error) {
	s.swept = true
	return 1, nil
}

// Add and Remove run while requests are routed: -race proves the snapshot.
func TestOrchestrator_AddRemoveConcurrentWithRouting(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	o := New(docker)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r := runnerSub(uuid.NewString())
			for range 50 {
				o.Add(r)
				o.Remove(r)
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				_, _ = o.Capabilities(context.Background())
				_, _ = o.CreateSandbox(context.Background(), specFor(types.CC1))
			}
		}()
	}
	wg.Wait()
}

// Replacing a runner's substrate (a reconnect builds a new one) must move its refs to the new object.
func TestOrchestrator_AddReplacesAndReroutesRefs(t *testing.T) {
	docker := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	oldSub, fresh := runnerSub("a"), runnerSub("a")
	o := New(docker)
	o.Add(oldSub)
	spec := specFor(types.CC1)
	spec.RunnerID = "a"
	sb, err := o.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	o.Add(fresh)
	if _, err := o.Status(context.Background(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if len(fresh.statuses) != 1 || len(oldSub.statuses) != 0 {
		t.Fatalf("status went to old=%d fresh=%d, want the replacement", len(oldSub.statuses), len(fresh.statuses))
	}
	if n := len(o.snapshot()); n != 2 {
		t.Fatalf("%d substrates after a replace, want 2", n)
	}
}
