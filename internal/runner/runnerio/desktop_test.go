// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// desktopFake is a substrate whose sandboxes have a display: it greets as an RFB server would, then
// echoes what the viewer sends.
type desktopFake struct {
	contextSubstrate
	dialled chan string
}

func (f desktopFake) DialDesktop(_ context.Context, ref string) (io.ReadWriteCloser, error) {
	f.dialled <- ref
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		if _, err := b.Write([]byte("RFB 003.008\n")); err != nil {
			return
		}
		_, _ = io.Copy(b, b)
	}()
	return a, nil
}

// The org opens a VM's display through the runner only once the runner's caps offer it; the stream
// then carries the display's bytes both ways, and a sandbox the runner does not hold is never dialled.
func TestDialDesktopCrossesTheWireOnlyWhenTheRunnerOffersIt(t *testing.T) {
	fake := runnertest.NewFake()
	local, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	other, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	sub := desktopFake{contextSubstrate{fake}, make(chan string, 4)}
	server, err := New(sub, "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	rs := remote.New("laptop", link, remote.Options{})
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return server.Call(ctx, rp, id, method, args)
	}})
	go rp.Run(t.Context())
	server.HoldSandboxes(func(ref string) bool { return ref == local.Ref })
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	ref := placement.RunnerRefPrefix("laptop") + local.Ref
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := rs.DialDesktop(ctx, ref); !errors.Is(err, runner.ErrDesktopUnsupported) {
		t.Fatalf("a runner whose caps do not offer the desktop = %v, want ErrDesktopUnsupported", err)
	}
	caps, _ := json.Marshal(runnerwire.Caps{LinuxVM: &runnerwire.VMCaps{Desktop: true}})
	rs.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})

	conn, err := rs.DialDesktop(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	greeting := make([]byte, 12)
	if _, err := io.ReadFull(conn, greeting); err != nil || string(greeting) != "RFB 003.008\n" {
		t.Fatalf("greeting = %q, %v", greeting, err)
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, greeting); err != nil || string(greeting) != "RFB 003.008\n" {
		t.Fatalf("echo = %q, %v", greeting, err)
	}
	if got := <-sub.dialled; got != local.Ref {
		t.Fatalf("dialled %q, want %q", got, local.Ref)
	}

	if c, err := rs.DialDesktop(ctx, placement.RunnerRefPrefix("laptop")+other.Ref); err == nil {
		_ = c.Close()
		t.Fatal("a sandbox the runner does not hold was dialled")
	}
	select {
	case got := <-sub.dialled:
		t.Fatalf("a refused desktop reached the substrate (%q)", got)
	default:
	}
}
