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

func bufferLossy(server *Server, id uuid.UUID) bool {
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lossy
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
	// The org signals its refusal before the RESET reaches the runner. Waiting for the runner's
	// mark starts the recovery after it, the order this test pins; a mark that would land after
	// the recovery's first ACK is TestOutputRecoveryAfterALateRefusalKeepsEveryByte's.
	for deadline := time.Now().Add(10 * time.Second); !bufferLossy(server, id); {
		if time.Now().After(deadline) {
			t.Fatal("runner never marked the refused output unacknowledged")
		}
		time.Sleep(time.Millisecond)
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

// recoveryAfterRestart writes prefix, connects a restarted org that refuses the runner's OPEN and
// starts its recovery of the run. It returns the run's writer and drain end, the capture and the
// recovery's result.
func recoveryAfterRestart(t *testing.T, server *Server, id uuid.UUID, prefix []byte) (io.Writer, func(error), *outputCapture, <-chan error) {
	t.Helper()
	local := "local-" + id.String()
	w, err := server.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.BindOutput(id, local); err != nil {
		t.Fatal(err)
	}
	end := runner.BeginOutputDrain(w)
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
	return w, end, capture, recovered
}

// writeAsync writes data and ends the drain with the write's error.
func writeAsync(w io.Writer, end func(error), data []byte) <-chan error {
	written := make(chan error, 1)
	go func() {
		_, err := w.Write(data)
		end(err)
		written <- err
	}()
	return written
}

// The runner can read the restarted org's refusal after the recovery has ACKed. That refusal is
// stale: marking the output unacknowledged then would let a full buffer drop bytes the recovery
// has not read yet.
func TestOutputRecoveryAfterALateRefusalKeepsEveryByte(t *testing.T) {
	server := outputServer(t)
	id := uuid.New()
	prefix := []byte("written before the org restarted\n")
	marked := make(chan struct{})
	server.markRefused = func(b *outputBuffer, acks uint64) {
		defer close(marked)
		for deadline := time.Now().Add(10 * time.Second); bufferState(server, id).Ack < int64(len(prefix)); {
			if time.Now().After(deadline) {
				t.Error("recovery never acknowledged the prefix")
				return
			}
			time.Sleep(time.Millisecond)
		}
		b.unacknowledgedSince(acks)
	}
	w, end, capture, recovered := recoveryAfterRestart(t, server, id, prefix)
	select {
	case <-marked:
	case <-time.After(time.Minute):
		t.Fatal("runner never handled the refusal")
	}
	more := bytes.Repeat([]byte("z"), outputLimit+outputLimit/8)
	if err := waitFor(t, "write past the buffer", writeAsync(w, end, more)); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(t, "recovery", recovered); err != nil {
		t.Fatalf("recovery after a late refusal: %v", err)
	}
	want := append(append([]byte{}, prefix...), more...)
	if !bytes.Equal(capture.Bytes(), want) {
		t.Fatalf("recovered %d bytes, want %d", capture.Len(), len(want))
	}
	if st := bufferState(server, id); !st.Complete || st.Ack != st.End {
		t.Fatalf("runner cursor=%+v", st)
	}
}

// A recovery whose bytes the runner evicted ends with the runner's reset of the stream, not with
// the context its failed ACK saw.
func TestOutputRecoveryResetReportsTheRunnersCause(t *testing.T) {
	server := outputServer(t)
	id := uuid.New()
	prefix := []byte("written before the org restarted\n")
	w, end, _, recovered := recoveryAfterRestart(t, server, id, prefix)
	for deadline := time.Now().Add(10 * time.Second); bufferState(server, id).Ack < int64(len(prefix)); {
		if time.Now().After(deadline) {
			t.Fatal("recovery never acknowledged the prefix")
		}
		time.Sleep(time.Millisecond)
	}
	// Force the eviction: a lossy buffer drops its oldest parts, which the recovery has not read.
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	b.mu.Lock()
	b.lossy = true
	b.mu.Unlock()
	if err := waitFor(t, "write past the buffer", writeAsync(w, end, bytes.Repeat([]byte("z"), outputLimit+outputLimit/8))); err != nil {
		t.Fatal(err)
	}
	err := waitFor(t, "recovery", recovered)
	var reset *runnerwire.ResetError
	if !errors.As(err, &reset) || reset.Code != runnerwire.ResetInternal {
		t.Fatalf("recovery ended with %v, want the runner's reset (code %d)", err, runnerwire.ResetInternal)
	}
}
