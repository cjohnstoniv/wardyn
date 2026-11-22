// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
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

// fillHandler is the org side of capacityRig: it accepts the relay streams the test opens from the
// runner to fill the stream table, answers the next refuse output OPENs with the capacity reset, and
// hands every other OPEN to the org Substrate.
type fillHandler struct {
	sub     *remote.Substrate
	refuse  atomic.Int32
	outputs atomic.Int32 // output OPENs that reached the org
}

func (h *fillHandler) HandleEvent(ev runnerwire.Event) { h.sub.HandleEvent(ev) }
func (h *fillHandler) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	switch o.Kind {
	case runnerwire.KindRelay:
		return nil
	case runnerwire.KindOutput:
		h.outputs.Add(1)
		if h.refuse.Add(-1) >= 0 {
			return runnerwire.ErrOpenCapacity
		}
	}
	return h.sub.HandleOpen(st, o)
}

// capacityRig is orgProcess with fillHandler on the org side, returning the runner's peer.
func capacityRig(t *testing.T, server *Server) (*remote.Substrate, *fillHandler, *runnerwire.Peer) {
	t.Helper()
	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	h := &fillHandler{sub: sub}
	link.SetHandler(h)
	caps, _ := json.Marshal(runnerwire.Caps{Support: runnertest.NewFake().Support})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	org, run := runnerwire.Loopback()
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, raw json.RawMessage) (any, error) {
		if method != runnerwire.MethodCreateSandbox {
			return server.Call(ctx, rp, id, method, raw)
		}
		var args runnerwire.CreateSandboxArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		if _, err := server.Output(args.Spec.RunID); err != nil {
			return nil, err
		}
		local := "local-" + args.Spec.RunID.String()
		if err := server.BindOutput(args.Spec.RunID, local); err != nil {
			return nil, err
		}
		return runnerwire.CreateSandboxResult{Sandbox: runner.Sandbox{Ref: placement.RunnerRefPrefix("laptop") + local, Driver: "docker"}}, nil
	}})
	go rp.Run(t.Context())
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	return sub, h, rp
}

// The output worker treats a full stream table on the runner's own side as it treats a full peer: both
// answer its Open with the same capacity reset, it retries, and once a place frees it opens and delivers.
func TestRunnerioOutput_LocalCapacityRetries(t *testing.T) {
	capacity := &runnerwire.ResetError{Code: runnerwire.ResetCapacity}
	for _, full := range []string{"local", "peer"} {
		t.Run(full, func(t *testing.T) {
			server := outputServer(t)
			sub, h, rp := capacityRig(t, server)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			var fillers []*runnerwire.Stream
			if full == "local" {
				for range runnerwire.MaxStreams {
					st, err := rp.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
					if err != nil {
						t.Fatalf("fill the stream table: %v", err)
					}
					fillers = append(fillers, st)
				}
			} else {
				h.refuse.Store(1)
			}
			probe, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
			_, err := rp.Open(probe, runnerwire.Open{Kind: runnerwire.KindOutput, Target: uuid.NewString()})
			cancelProbe()
			if !reflect.DeepEqual(err, capacity) {
				t.Fatalf("Open with the table full (%s) = %#v, want %#v", full, err, capacity)
			}
			if full == "local" && h.outputs.Load() != 0 {
				t.Fatal("the local refusal sent an OPEN")
			}
			if full == "peer" {
				h.refuse.Store(2) // the worker's first two OPENs are refused too
			}

			id := uuid.New()
			capture := &outputCapture{done: make(chan struct{})}
			if _, err := sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: id, ExecOutput: capture}); err != nil {
				t.Fatal(err)
			}
			want := []byte("output that waited for a place\n")
			drained := drain(t, server, id, want)
			time.Sleep(300 * time.Millisecond) // the worker tries, is refused and backs off
			select {
			case <-capture.done:
				t.Fatal("output was delivered while the table was full")
			default:
			}
			if full == "local" {
				if n := h.outputs.Load(); n != 0 {
					t.Fatalf("%d output OPENs reached the org while the runner's table was full", n)
				}
				_ = fillers[0].Close() // one place frees
			}
			if err := waitFor(t, "the drain", drained); err != nil {
				t.Fatal(err)
			}
			select {
			case <-capture.done:
			case <-time.After(15 * time.Second):
				t.Fatal("the output worker stopped instead of retrying: its output never arrived")
			}
			if capture.err != nil || !bytes.Equal(capture.Bytes(), want) {
				t.Fatalf("org output %q, %v; want %q", capture.Bytes(), capture.err, want)
			}
			if full == "peer" && h.outputs.Load() < 3 {
				t.Fatalf("%d output OPENs, want the probe's, two refused and the accepted one", h.outputs.Load())
			}
		})
	}
}
