// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// refusalWatch reports each output OPEN the org refused.
type refusalWatch struct {
	sub     *remote.Substrate
	refused chan struct{}
}

func (h *refusalWatch) HandleEvent(ev runnerwire.Event) { h.sub.HandleEvent(ev) }
func (h *refusalWatch) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	err := h.sub.HandleOpen(st, o)
	if err != nil {
		h.refused <- struct{}{}
	}
	return err
}

// orgProcess connects server to a fresh org Substrate, as a wardynd that has
// just started sees the runner. A create binds output to "local-<run>" without
// writing any, so the test drives the bytes.
func orgProcess(t *testing.T, server *Server) (*remote.Substrate, *refusalWatch) {
	t.Helper()
	return orgProcessWith(t, server, remote.Options{})
}

// orgProcessWith is orgProcess with the org Substrate's options.
func orgProcessWith(t *testing.T, server *Server, opts remote.Options) (*remote.Substrate, *refusalWatch) {
	t.Helper()
	link := &remote.Link{}
	sub := remote.New("laptop", link, opts)
	watch := &refusalWatch{sub: sub, refused: make(chan struct{}, 16)}
	link.SetHandler(watch)
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
	return sub, watch
}

func outputServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(runnertest.NewFake(), "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// drain copies data into the run's writer the way a driver does and reports
// the drain's end.
func drain(t *testing.T, server *Server, id uuid.UUID, data []byte) <-chan error {
	t.Helper()
	w, err := server.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	end := runner.BeginOutputDrain(w)
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(w, bytes.NewReader(data))
		end(err)
		done <- err
	}()
	return done
}

func waitFor(t *testing.T, what string, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Minute):
		t.Fatalf("%s never finished", what)
		return nil
	}
}

func bufferState(server *Server, id uuid.UUID) outputState {
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// With the org's sink present and ACKs flowing, more than the runner's whole
// buffer passes through without a byte lost or a gap reported.
func TestOutputAckedCreatePathKeepsEveryByte(t *testing.T) {
	server := outputServer(t)
	sub, _ := orgProcess(t, server)
	id := uuid.New()
	capture := &outputCapture{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: id, ExecOutput: capture}); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("0123456789abcdef"), (outputLimit+outputLimit/8)/16)
	if err := waitFor(t, "acked drain", drain(t, server, id, want)); err != nil {
		t.Fatal(err)
	}
	// Each 64 KiB part is written, and each ACK recorded, through fsync, so how long the transfer
	// takes is the host's disk and CPU, not the property under test: the bound on the org's end of
	// it is a hang guard, as waitFor's is, and not the 10 s that bounds the create call.
	select {
	case <-capture.done:
	case <-time.After(time.Minute):
		t.Fatal("org output never completed")
	}
	if capture.err != nil || !bytes.Equal(capture.Bytes(), want) {
		t.Fatalf("org output=%d bytes err=%v, want %d", capture.Len(), capture.err, len(want))
	}
	if st := bufferState(server, id); !st.Complete || st.Ack != st.End || st.End != int64(len(want)) {
		t.Fatalf("runner cursor=%+v", st)
	}
}

// An org with no sink for the run (it restarted) refuses the output OPEN.
// The agent's writes must still return, its drain must end, and the prefix
// dropped to make room reads as unrecoverable.
func TestOutputRefusedOpenNeverParksTheAgent(t *testing.T) {
	server := outputServer(t)
	sub, watch := orgProcess(t, server)
	id := uuid.New()
	local := "local-" + id.String()
	done := drain(t, server, id, bytes.Repeat([]byte("y"), outputLimit+outputLimit/8))
	if err := server.BindOutput(id, local); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watch.refused:
	case <-time.After(10 * time.Second):
		t.Fatal("org never refused the output OPEN")
	}
	if err := waitFor(t, "drain with no acker", done); err != nil {
		t.Fatal(err)
	}
	if st := bufferState(server, id); !st.Complete || st.End != outputLimit+outputLimit/8 {
		t.Fatalf("drain did not reach its end: %+v", st)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var recovered bytes.Buffer
	if err := sub.RecoverOutput(ctx, placement.RunnerRefPrefix("laptop")+local, &recovered); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("recovery of a dropped prefix=%v (%d bytes)", err, recovered.Len())
	}
}

// A wardynd that adopts a live run after a restart acknowledges what it
// recovers, so the runner keeps streaming past its whole buffer.
func TestOutputRecoveryAfterOrgRestartAcknowledges(t *testing.T) {
	server := outputServer(t)
	id := uuid.New()
	local := "local-" + id.String()
	w, err := server.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.BindOutput(id, local); err != nil {
		t.Fatal(err)
	}
	end := runner.BeginOutputDrain(w)
	prefix := []byte("written before the org restarted\n")
	if _, err = w.Write(prefix); err != nil {
		t.Fatal(err)
	}
	sub, watch := orgProcess(t, server)
	select {
	case <-watch.refused:
	case <-time.After(10 * time.Second):
		t.Fatal("restarted org never refused the output OPEN")
	}
	capture := &outputCapture{done: make(chan struct{})}
	recovered := make(chan error, 1)
	ctx := runner.WithOutputRun(t.Context(), id)
	go func() { recovered <- sub.RecoverOutput(ctx, placement.RunnerRefPrefix("laptop")+local, capture) }()
	for deadline := time.Now().Add(10 * time.Second); bufferState(server, id).Ack < int64(len(prefix)); {
		if time.Now().After(deadline) {
			t.Fatal("recovery never acknowledged the prefix")
		}
		time.Sleep(time.Millisecond)
	}
	more := bytes.Repeat([]byte("z"), outputLimit+outputLimit/8)
	written := make(chan error, 1)
	go func() {
		_, err := w.Write(more)
		end(err)
		written <- err
	}()
	if err = waitFor(t, "write past the buffer", written); err != nil {
		t.Fatal(err)
	}
	if err = waitFor(t, "recovery", recovered); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, prefix...), more...)
	if !bytes.Equal(capture.Bytes(), want) {
		t.Fatalf("recovered %d bytes, want %d", capture.Len(), len(want))
	}
	if st := bufferState(server, id); !st.Complete || st.Ack != st.End {
		t.Fatalf("runner cursor=%+v", st)
	}
}
