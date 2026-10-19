// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// adoptCapture is an adopting org's writer: it marks gaps and closes held once it holds heldLen bytes.
type adoptCapture struct {
	outputCapture
	skipped  []int64
	heldLen  int
	held     chan struct{}
	heldOnce sync.Once
}

func (c *adoptCapture) OutputGap(skipped int64) { c.skipped = append(c.skipped, skipped) }

func (c *adoptCapture) Write(p []byte) (int, error) {
	n, err := c.outputCapture.Write(p)
	if c.Len() >= c.heldLen {
		c.heldOnce.Do(func() { close(c.held) })
	}
	return n, err
}

// openResults reports what the org answered each output OPEN.
type openResults struct {
	sub   *remote.Substrate
	opens chan error
}

func (h *openResults) HandleEvent(ev runnerwire.Event) { h.sub.HandleEvent(ev) }
func (h *openResults) HandleOpen(st *runnerwire.Stream, o runnerwire.Open) error {
	err := h.sub.HandleOpen(st, o)
	if o.Kind == runnerwire.KindOutput {
		select {
		case h.opens <- err:
		default:
		}
	}
	return err
}

// testAdoption adopts a live run's output after an org restart. With compete, the runner sends its
// OPEN (from its acknowledged offset, as a reconnect does) after the org has registered the adopted
// sink and before the recovery reply. The bytes must arrive once each, in order, after any gap.
func testAdoption(t *testing.T, ackFirst, compete bool) {
	server, err := New(runnertest.NewFake(), "laptop", lateWriterDir(t))
	if err != nil {
		t.Fatal(err)
	}
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
	p1 := []byte("acknowledged by the org that went away\n")
	p2 := []byte("written while no org was listening\n")
	p3 := []byte("written after the adoption\n")
	if _, err = w.Write(p1); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	want, gaps := slices.Concat(p1, p2, p3), []int64(nil)
	if ackFirst {
		// The org that went away acknowledged p1 and the runner discarded it.
		if err = b.ack(int64(len(p1))); err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		err = b.evict(false)
		start := b.start()
		b.mu.Unlock()
		if err != nil || start != int64(len(p1)) {
			t.Fatalf("evict: start %d, %v", start, err)
		}
		want, gaps = slices.Concat(p2, p3), []int64{int64(len(p1))}
	}
	if _, err = w.Write(p2); err != nil {
		t.Fatal(err)
	}
	capture := &adoptCapture{outputCapture: outputCapture{done: make(chan struct{})}, heldLen: len(want) - len(p3), held: make(chan struct{})}

	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	watch := &openResults{sub: sub, opens: make(chan error, 64)}
	link.SetHandler(watch)
	caps, _ := json.Marshal(runnerwire.Caps{Support: runnertest.NewFake().Support})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	org, run := runnerwire.Loopback()
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, cid uint32, method string, raw json.RawMessage) (any, error) {
		if method == runnerwire.MethodRecoverOutput && compete {
			server.sendOutput(rp, id, b)
			// Let the competing stream make its move before the reply: refused, or (accepted)
			// until it has ended the sink or written all it had.
			if err := <-watch.opens; err == nil {
				select {
				case <-capture.done:
				case <-capture.held:
				case <-time.After(10 * time.Second):
					t.Error("the competing stream never moved")
				}
			}
		}
		return server.Call(ctx, rp, cid, method, raw)
	}})
	go rp.Run(t.Context())
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	select {
	case err := <-watch.opens:
		if err == nil {
			t.Fatal("an org with no sink accepted the connect OPEN")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the connect OPEN never reached the org")
	}

	ctx, cancel := context.WithTimeout(runner.WithOutputRun(t.Context(), id), 20*time.Second)
	defer cancel()
	recovered := make(chan error, 1)
	go func() { recovered <- sub.RecoverOutput(ctx, placement.RunnerRefPrefix("laptop")+local, capture) }()
	select {
	case <-capture.held:
	case err := <-recovered:
		t.Fatalf("adoption ended before the retained bytes: %v (%q)", err, capture.String())
	case <-ctx.Done():
		t.Fatal("the retained bytes never arrived")
	}
	// The OPEN of a later reconnect is accepted into the adopted sink.
	server.sendOutput(rp, id, b)
	for accepted := false; !accepted; {
		select {
		case err := <-watch.opens:
			accepted = err == nil
		case <-ctx.Done():
			t.Fatal("no later OPEN was accepted into the adopted sink")
		}
	}
	if _, err = w.Write(p3); err != nil {
		t.Fatal(err)
	}
	end(nil)
	if err = waitFor(t, "recovery", recovered); err != nil {
		t.Fatalf("RecoverOutput = %v (%q)", err, capture.String())
	}
	if !bytes.Equal(capture.Bytes(), want) || !slices.Equal(capture.skipped, gaps) || capture.err != nil {
		t.Fatalf("adopted %q, gaps %v, end %v; want %q, gaps %v, end nil", capture.String(), capture.skipped, capture.err, want, gaps)
	}
	if st := bufferState(server, id); !st.Complete || st.Ack != st.End {
		t.Fatalf("runner cursor=%+v", st)
	}
}

// A runner OPEN from an acknowledged offset above 0 during the adoption does not end the adopted
// sink: the gap is marked, then every retained and live byte arrives.
func TestOutputAdoption_CompetingOpenAboveZeroAck(t *testing.T) { testAdoption(t, true, true) }

// A runner OPEN at offset 0 during the adoption does not write the retained prefix twice.
func TestOutputAdoption_CompetingOpenAtZeroAck(t *testing.T) { testAdoption(t, false, true) }

// An adoption with no competing OPEN delivers every byte once, as before.
func TestOutputAdoption_NoCompetingOpen(t *testing.T) { testAdoption(t, false, false) }

// slowCapture is an adoptCapture whose writes take perWrite each; onFirst runs on the first one.
type slowCapture struct {
	adoptCapture
	first    sync.Once
	onFirst  func()
	perWrite time.Duration
}

func (c *slowCapture) Write(p []byte) (int, error) {
	c.first.Do(c.onFirst)
	time.Sleep(c.perWrite)
	return c.adoptCapture.Write(p)
}

// The usual shape after an org restart: the runner keeps every acknowledged byte (parts are evicted
// only when its buffer is full), so recovery re-reads from 0 while the runner's OPEN, sent as soon as
// the recovery begins writing, starts at its Ack. That OPEN must wait for the recovery stream to catch
// up rather than end the adopted sink with a forward gap.
func TestOutputAdoption_OpenAheadOfCatchUp(t *testing.T) {
	server, err := New(runnertest.NewFake(), "laptop", lateWriterDir(t))
	if err != nil {
		t.Fatal(err)
	}
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
	p1 := bytes.Repeat([]byte("a"), 2<<20) // acknowledged by the org that went away; 32 chunks
	p2 := []byte("written while no org was listening\n")
	p3 := []byte("written after the adoption\n")
	if _, err = w.Write(p1); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	if err = b.ack(int64(len(p1))); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	start, ack := b.start(), b.state.Ack
	b.mu.Unlock()
	if start != 0 || ack != int64(len(p1)) {
		t.Fatalf("start %d ack %d", start, ack)
	}
	if _, err = w.Write(p2); err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(p1, p2, p3)
	capture := &slowCapture{adoptCapture: adoptCapture{outputCapture: outputCapture{done: make(chan struct{})}, heldLen: len(p1) + len(p2), held: make(chan struct{})}, perWrite: 5 * time.Millisecond}

	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	watch := &openResults{sub: sub, opens: make(chan error, 64)}
	link.SetHandler(watch)
	caps, _ := json.Marshal(runnerwire.Caps{Support: runnertest.NewFake().Support})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	org, run := runnerwire.Loopback()
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, cid uint32, method string, raw json.RawMessage) (any, error) {
		return server.Call(ctx, rp, cid, method, raw)
	}})
	// The runner's OPEN from its Ack lands once the recovery has begun writing (pending is clear).
	capture.onFirst = func() { server.sendOutput(rp, id, b) }
	go rp.Run(t.Context())
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	select {
	case err := <-watch.opens:
		if err == nil {
			t.Fatal("an org with no sink accepted the connect OPEN")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the connect OPEN never reached the org")
	}

	ctx, cancel := context.WithTimeout(runner.WithOutputRun(t.Context(), id), 40*time.Second)
	defer cancel()
	recovered := make(chan error, 1)
	go func() { recovered <- sub.RecoverOutput(ctx, placement.RunnerRefPrefix("laptop")+local, capture) }()
	select {
	case <-capture.held:
	case err := <-recovered:
		t.Fatalf("adoption ended before the retained bytes: %v (got %d bytes)", err, capture.Len())
	case <-ctx.Done():
		t.Fatal("the retained bytes never arrived")
	}
	// The runner's OPEN, sent as the recovery began writing, reached the org while the sink was still
	// catching up: it is turned away with a capacity reset (the runner retries), never refused or
	// failed, which would end the adopted sink with a forward gap. The sink now holds every byte the
	// runner acknowledged, so a retry must be accepted, whenever its backoff lands it. Once the recovery
	// ends the sink is gone and a later retry is refused, so only results before the accept count.
	acceptBy := time.After(10 * time.Second)
	for accepted := false; !accepted; {
		var err error
		select {
		case err = <-watch.opens:
		case <-acceptBy:
			end(nil) // let the recovery finish, so the failure is reported rather than a hang
			t.Fatal("the runner's retried OPEN was never accepted into the adopted sink")
		}
		if err != nil && !errors.Is(err, runnerwire.ErrOpenCapacity) {
			t.Fatalf("the OPEN ahead of the catch-up was answered %v; want a capacity reset, then an accept", err)
		}
		accepted = err == nil
	}
	if _, err = w.Write(p3); err != nil {
		t.Fatal(err)
	}
	end(nil)
	if err = waitFor(t, "recovery", recovered); err != nil {
		t.Fatalf("RecoverOutput = %v (got %d bytes)", err, capture.Len())
	}
	if !bytes.Equal(capture.Bytes(), want) || capture.err != nil {
		t.Fatalf("adopted %d bytes, end %v; want %d bytes, end nil", capture.Len(), capture.err, len(want))
	}
}

// The recovery resumes at the first retained part's start, and a part is evicted only once its end
// is at or below Ack, so Ack can sit inside a retained part, above the resume point. The runner's
// OPEN from that Ack, sent as the recovery begins writing, is held like one at a part boundary
// (HandleOpen compares byte offsets, not parts): the bytes from the resume point arrive once, after
// one gap, and the adopted sink never ends with a forward gap.
func TestOutputAdoption_OpenFromAnAckInsideARetainedPart(t *testing.T) {
	server, err := New(runnertest.NewFake(), "laptop", lateWriterDir(t))
	if err != nil {
		t.Fatal(err)
	}
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
	p1 := bytes.Repeat([]byte("a"), 2<<20) // 32 parts of outputChunk
	p2 := []byte("written while no org was listening\n")
	p3 := []byte("written after the adoption\n")
	if _, err = w.Write(p1); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	if err = b.ack(1<<20 + 100); err != nil { // 100 bytes into the 17th part
		t.Fatal(err)
	}
	b.mu.Lock()
	for before := int64(-1); before != b.start(); {
		before = b.start()
		if err = b.evict(false); err != nil {
			break
		}
	}
	start, ack := b.start(), b.state.Ack
	b.mu.Unlock()
	if err != nil || start != 1<<20 || ack != start+100 {
		t.Fatalf("evict: start %d ack %d, %v; want the Ack 100 bytes inside the first retained part", start, ack, err)
	}
	if _, err = w.Write(p2); err != nil {
		t.Fatal(err)
	}
	all := slices.Concat(p1, p2, p3)
	want := all[start:]
	capture := &slowCapture{adoptCapture: adoptCapture{outputCapture: outputCapture{done: make(chan struct{})}, heldLen: len(want) - len(p3), held: make(chan struct{})}, perWrite: 5 * time.Millisecond}

	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	watch := &openResults{sub: sub, opens: make(chan error, 64)}
	link.SetHandler(watch)
	caps, _ := json.Marshal(runnerwire.Caps{Support: runnertest.NewFake().Support})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	org, run := runnerwire.Loopback()
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, cid uint32, method string, raw json.RawMessage) (any, error) {
		return server.Call(ctx, rp, cid, method, raw)
	}})
	capture.onFirst = func() { server.sendOutput(rp, id, b) }
	go rp.Run(t.Context())
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	select {
	case err := <-watch.opens:
		if err == nil {
			t.Fatal("an org with no sink accepted the connect OPEN")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the connect OPEN never reached the org")
	}

	ctx, cancel := context.WithTimeout(runner.WithOutputRun(t.Context(), id), 40*time.Second)
	defer cancel()
	recovered := make(chan error, 1)
	go func() { recovered <- sub.RecoverOutput(ctx, placement.RunnerRefPrefix("laptop")+local, capture) }()
	select {
	case <-capture.held:
	case err := <-recovered:
		t.Fatalf("adoption ended before the retained bytes: %v (got %d bytes)", err, capture.Len())
	case <-ctx.Done():
		t.Fatal("the retained bytes never arrived")
	}
	var first error
	select {
	case first = <-watch.opens:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner's OPEN from inside the retained part never reached the org")
	}
	for err := first; ; err = <-watch.opens {
		if err != nil && !errors.Is(err, runnerwire.ErrOpenCapacity) {
			t.Fatalf("the OPEN from inside the retained part was answered %v; want a capacity reset or an accept", err)
		}
		if len(watch.opens) == 0 {
			break
		}
	}
	if _, err = w.Write(p3); err != nil {
		t.Fatal(err)
	}
	end(nil)
	if err = waitFor(t, "recovery", recovered); err != nil {
		t.Fatalf("RecoverOutput = %v (got %d bytes)", err, capture.Len())
	}
	if !bytes.Equal(capture.Bytes(), want) || !slices.Equal(capture.skipped, []int64{start}) || capture.err != nil {
		t.Fatalf("adopted %d bytes, gaps %v, end %v; want %d bytes from %d, gaps [%d], end nil", capture.Len(), capture.skipped, capture.err, len(want), start, start)
	}
}
