// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

type contextSubstrate struct{ *runnertest.Fake }

func (f contextSubstrate) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	e, err := f.Fake.ExecStream(ctx, ref, spec)
	if err == nil {
		context.AfterFunc(ctx, func() { _ = e.Close() })
	}
	return e, err
}

func streamRig(t *testing.T) (*Server, *remote.Substrate, *runnertest.Fake, *runnerwire.Peer, string) {
	t.Helper()
	fake := runnertest.NewFake()
	fake.ExitCode = 7
	local, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(contextSubstrate{fake}, "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return server.Call(ctx, rp, id, method, args)
	}})
	go rp.Run(t.Context())
	server.HoldSandboxes(func(ref string) bool { return ref == local.Ref })
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	return server, sub, fake, rp, placement.RunnerRefPrefix("laptop") + local.Ref
}

// Only a sandbox this runner holds is reachable by attach or exec, even when
// the substrate can see another container under that name.
func TestStreamsRefuseSandboxesTheRunnerDoesNotHold(t *testing.T) {
	_, sub, fake, _, held := streamRig(t)
	other, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	foreign := placement.RunnerRefPrefix("laptop") + other.Ref
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if e, err := sub.ExecStream(ctx, foreign, runner.ExecSpec{Argv: []string{"cat"}}); err == nil {
		_ = e.Close()
		t.Fatal("exec reached a sandbox the runner does not hold")
	}
	if a, err := sub.Attach(ctx, foreign, runner.AttachOptions{}); err == nil {
		_ = a.Close()
		t.Fatal("attach reached a sandbox the runner does not hold")
	}
	if fake.Called("exec_stream", other.Ref) || fake.Called("attach", other.Ref) {
		t.Fatal("a refused stream reached the substrate")
	}
	a, err := sub.Attach(ctx, held, runner.AttachOptions{})
	if err != nil {
		t.Fatalf("held sandbox attach: %v", err)
	}
	_ = a.Close()
}

func TestExecReplyKeepsStreamsAliveAndHalfCloseDrains(t *testing.T) {
	server, sub, fake, _, ref := streamRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e, err := sub.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	want := bytes.Repeat([]byte("a complete stdout chunk\n"), 20000)
	type result struct {
		data []byte
		err  error
	}
	stdout, stderr := make(chan result, 1), make(chan result, 1)
	go func() { b, e := io.ReadAll(e.Stdout); stdout <- result{b, e} }()
	go func() { b, e := io.ReadAll(e.Stderr); stderr <- result{b, e} }()
	if err = e.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Stdin.Write(want); err != nil {
		t.Fatal(err)
	}
	if err = e.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	var out, other result
	select {
	case out = <-stdout:
	case <-ctx.Done():
		t.Fatal("stdout stuck after half-close")
	}
	select {
	case other = <-stderr:
	case <-ctx.Done():
		t.Fatal("stderr stuck after half-close")
	}
	if out.err != nil || !bytes.Equal(out.data, want) || other.err != nil || string(other.data) != "argv:cat\n" {
		t.Fatalf("stream mismatch stdout=%d,%v stderr=%q,%v", len(out.data), out.err, other.data, other.err)
	}
	code, err := e.Wait()
	if err != nil || code != 7 {
		t.Fatalf("wait=%d,%v", code, err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	waitExecsDrained(t, server, "exec close leaked state")
	local, _ := server.local(ref)
	if fake.Called("kill", local) || fake.Called("stop", local) {
		t.Fatal("closing exec stopped sandbox")
	}
}

func TestPeerLossClosesExecWithoutStoppingSandbox(t *testing.T) {
	server, sub, fake, peer, ref := streamRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	e, err := sub.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	peer.Close()
	server.Offline(peer)
	if _, err = e.Stdin.Write([]byte("after disconnect")); err == nil {
		t.Fatal("disconnected stdin accepted bytes")
	}
	waitExecsDrained(t, server, "disconnect leaked execs")
	local, _ := server.local(ref)
	if _, err = fake.Status(ctx, local); err != nil {
		t.Fatalf("sandbox lost on stream disconnect: %v", err)
	}
}

// waitExecsDrained waits for every exec and its slot to be gone. removeExec drops
// the map entry before it frees the slot, so a copy goroutine that already won the
// race can still be mid-close when the call under test returns.
func waitExecsDrained(t *testing.T, server *Server, what string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		server.mu.Lock()
		left, slots := len(server.execs), len(server.execSlots)
		server.mu.Unlock()
		if left == 0 && slots == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d execs, %d slots", what, left, slots)
		}
	}
}
