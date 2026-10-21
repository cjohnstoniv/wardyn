// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// While an adopted sink's recovery stream is catching up, a runner OPEN from an offset the sink has
// not reached is turned away with a capacity reset (the runner retries); one at or below the sink's
// offset, or once recovery is over, is accepted and dedupes through accept's skip.
func TestHandleOpenDefersAnOpenAheadOfTheRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	link := &Link{}
	s := New("r1", link, Options{})
	org, run := runnerwire.Loopback()
	op := link.Attach(ctx, org)
	rp := runnerwire.NewPeer(run, runnerwire.PeerConfig{})
	go rp.Run(ctx)
	t.Cleanup(func() { rp.Close(); op.Close() })
	for _, tc := range []struct {
		name               string
		offset, open       int64
		recovering, capped bool
	}{
		{"ahead while recovering", 0, 1, true, true},
		// The recovery resumes at the first retained part's start while the runner's Ack sits inside
		// that part: the OPEN from the Ack is ahead by bytes, whatever the part boundaries.
		{"from an Ack inside the first retained part", 1 << 20, 1<<20 + 100, true, true},
		{"ahead once recovered", 0, 1, false, false},
		{"at the sink while recovering", 5, 5, true, false},
		{"behind the sink while recovering", 5, 3, true, false},
	} {
		id := uuid.New()
		s.mu.Lock()
		s.outputs[id] = &outputSink{writer: &bytes.Buffer{}, offset: tc.offset, recovering: tc.recovering}
		s.mu.Unlock()
		st, err := rp.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: id.String(), Offset: tc.open})
		var re *runnerwire.ResetError
		capped := errors.As(err, &re) && re.Code == runnerwire.ResetCapacity
		if capped != tc.capped || (!tc.capped && err != nil) {
			t.Errorf("%s: open = %v, want capacity reset %v", tc.name, err, tc.capped)
		}
		if st != nil {
			_ = st.Close()
		}
	}
}

// offlineAckLink is a Link whose output ACK is in flight when the link drops: it waits for the
// stream's end, then calls through the dead link.
type offlineAckLink struct {
	*Link
	acking chan struct{}
}

func (l *offlineAckLink) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	if method == runnerwire.MethodOutputAck {
		close(l.acking)
		<-ctx.Done()
		return l.Link.Call(context.Background(), method, args, result, opts...)
	}
	return l.Link.Call(ctx, method, args, result, opts...)
}

// streamCatch accepts the runner's output OPEN and hands its stream to the test.
type streamCatch chan *runnerwire.Stream

func (c streamCatch) HandleEvent(runnerwire.Event) {}
func (c streamCatch) HandleOpen(st *runnerwire.Stream, _ runnerwire.Open) error {
	c <- st
	return nil
}

// A link that drops under an output copy fails its ACK offline: the copy keeps that cause, which
// marks a gap a later recovery replaces, rather than the stream's reconnect reset.
func TestOutputAckOfflineOnALinkDropStaysOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	link := &offlineAckLink{Link: &Link{}, acking: make(chan struct{})}
	s := New("r1", link, Options{})
	streams := make(streamCatch, 1)
	link.SetHandler(streams)
	org, run := runnerwire.Loopback()
	op := link.Attach(ctx, org)
	rp := runnerwire.NewPeer(run, runnerwire.PeerConfig{})
	go rp.Run(ctx)
	t.Cleanup(func() { rp.Close(); op.Close() })
	id := uuid.New()
	rst, err := rp.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: id.String()})
	if err != nil {
		t.Fatal(err)
	}
	st := <-streams
	done := make(chan error, 1)
	go func() { done <- s.receiveOutput(id, &outputSink{writer: &bytes.Buffer{}}, st, 0) }()
	if _, err = rst.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-link.acking:
	case <-ctx.Done():
		t.Fatal("the copy never ACKed")
	}
	op.Close()
	err = <-done
	var reset *runnerwire.ResetError
	if !errors.As(st.Err(), &reset) || reset.Code != runnerwire.ResetReconnect {
		t.Fatalf("stream ended with %v, want a reconnect reset", st.Err())
	}
	if !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("copy ended with %v, want runner.ErrRunnerOffline", err)
	}
}
