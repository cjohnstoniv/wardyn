// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

type outputCapture struct {
	bytes.Buffer
	done chan struct{}
	once sync.Once
	err  error
}

func (c *outputCapture) BeginDrain()        {}
func (c *outputCapture) EndDrain(err error) { c.once.Do(func() { c.err = err; close(c.done) }) }

func TestOutputLostAcknowledgmentReplaysWithoutDuplicateBytes(t *testing.T) {
	for _, mode := range []string{"reconnect", "live", "capacity", "permanent"} {
		t.Run(mode, func(t *testing.T) { testOutputReplay(t, mode) })
	}
}

type capacityOutputHandler struct {
	sub      *remote.Substrate
	attempts atomic.Int32
}

func (h *capacityOutputHandler) HandleEvent(ev runnerwire.Event) { h.sub.HandleEvent(ev) }
func (h *capacityOutputHandler) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	if h.attempts.Add(1) == 1 {
		_ = st.Reset(runnerwire.ResetCapacity)
		return errors.New("injected stream capacity refusal")
	}
	return h.sub.HandleOpen(st, o)
}

func testOutputReplay(t *testing.T, mode string) {
	fake := runnertest.NewFake()
	server, err := New(fake, "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	capacity := &capacityOutputHandler{sub: sub}
	if mode == "capacity" {
		link.SetHandler(capacity)
	}
	caps, _ := json.Marshal(runnerwire.Caps{Support: fake.Support})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	var failed atomic.Bool
	var ackCalls atomic.Int32
	lost := make(chan struct{}, 1)
	connect := func() *runnerwire.Peer {
		org, run := runnerwire.Loopback()
		op := link.Attach(t.Context(), org)
		var rp *runnerwire.Peer
		rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, raw json.RawMessage) (any, error) {
			if method == runnerwire.MethodOutputAck {
				ackCalls.Add(1)
				if mode == "permanent" {
					return nil, runnerwire.Refuse("output disk is stopped")
				}
				if !failed.Swap(true) {
					lost <- struct{}{}
					return nil, runner.ErrRunnerOffline
				}
			}
			if method != runnerwire.MethodCreateSandbox {
				return server.Call(ctx, rp, id, method, raw)
			}
			var args runnerwire.CreateSandboxArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, err
			}
			if args.ExecOutput {
				w, err := server.Output(args.Spec.RunID)
				if err != nil {
					return nil, err
				}
				args.Spec.ExecOutput = w
			}
			sb, err := fake.CreateSandbox(ctx, args.Spec)
			if err != nil {
				return nil, err
			}
			if err = server.BindOutput(args.Spec.RunID, sb.Ref); err != nil {
				return nil, err
			}
			sb.Ref = placement.RunnerRefPrefix("laptop") + sb.Ref
			return runnerwire.CreateSandboxResult{Sandbox: sb}, nil
		}})
		go rp.Run(t.Context())
		server.Online(rp)
		t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
		return rp
	}
	first := connect()
	capture := &outputCapture{done: make(chan struct{})}
	id := uuid.New()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sb, err := sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: id, ExecOutput: capture})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "permanent" {
		select {
		case <-lost:
		case <-ctx.Done():
			t.Fatal("output was never accepted before ACK loss")
		}
	}
	if mode == "reconnect" {
		first.Close()
		server.Offline(first)
		connect()
	}
	select {
	case <-capture.done:
	case <-ctx.Done():
		t.Fatal("reconnected output never completed")
	}
	if mode == "permanent" {
		if capture.err == nil || ackCalls.Load() != 1 {
			t.Fatalf("permanent ACK failure err=%v attempts=%d", capture.err, ackCalls.Load())
		}
		return
	}
	if mode != "reconnect" && !first.Online() {
		t.Fatal("output retry disconnected a live peer")
	}
	if mode == "capacity" && capacity.attempts.Load() != 2 {
		t.Fatalf("open attempts=%d", capacity.attempts.Load())
	}
	if capture.err != nil {
		t.Fatal(capture.err)
	}
	want := "agent output of fake-" + id.String() + "\n"
	if capture.String() != want {
		t.Fatalf("output=%q want%q", capture.String(), want)
	}
	var recovered bytes.Buffer
	if err = sub.RecoverOutput(ctx, sb.Ref, &recovered); err != nil || recovered.String() != want {
		t.Fatalf("recovery=%q,%v", recovered.String(), err)
	}
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	b.mu.Lock()
	ack, end := b.state.Ack, b.state.End
	b.mu.Unlock()
	if ack != end || ack != int64(len(want)) {
		t.Fatalf("accepted cursor=%d/%d", ack, end)
	}
	if _, err = fake.Status(ctx, "fake-"+id.String()); err != nil {
		t.Fatalf("reconnect stopped sandbox: %v", err)
	}
}
