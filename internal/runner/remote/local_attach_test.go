// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// localRunner is a runner end that serves the local attach calls and records them.
type localRunner struct {
	mu      sync.Mutex
	peer    *runnerwire.Peer
	calls   []string
	modes   []runnerwire.LocalAttachModeArgs
	acks    []runnerwire.LocalAttachTeeAckArgs
	kind    string // the kind of stream local_attach answers with
	modeErr error
	tee     *runnerwire.Stream
}

func (l *localRunner) handle(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
	l.mu.Lock()
	l.calls = append(l.calls, method)
	l.mu.Unlock()
	switch method {
	case runnerwire.MethodLocalAttach:
		st, err := l.peer.Open(ctx, runnerwire.Open{Kind: l.kind, Call: id})
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		l.tee = st
		l.mu.Unlock()
		return runnerwire.StreamResult{Stream: st.ID()}, nil
	case runnerwire.MethodLocalAttachMode:
		var a runnerwire.LocalAttachModeArgs
		_ = json.Unmarshal(args, &a)
		l.mu.Lock()
		l.modes = append(l.modes, a)
		l.mu.Unlock()
		return nil, l.modeErr
	case runnerwire.MethodLocalAttachTeeAck:
		var a runnerwire.LocalAttachTeeAckArgs
		_ = json.Unmarshal(args, &a)
		l.mu.Lock()
		l.acks = append(l.acks, a)
		l.mu.Unlock()
		return nil, nil
	}
	return nil, runnerwire.Refuse("unknown runner method")
}

func (l *localRunner) called() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// localRig connects an org Substrate to a localRunner whose caps carry (or lack) LocalAttach.
func localRig(t *testing.T, capLocal bool) (*remote.Substrate, *localRunner, string) {
	t.Helper()
	var link remote.Link
	sub := remote.New(rid, &link, remote.Options{})
	org, run := runnerwire.Loopback()
	lr := &localRunner{kind: runnerwire.KindPTYTee}
	lr.peer = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: lr.handle})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	peer := link.Attach(ctx, org)
	go func() { _ = lr.peer.Run(ctx) }()
	t.Cleanup(func() { peer.Close(); lr.peer.Close() })
	caps, _ := json.Marshal(runnerwire.Caps{Version: "test", LocalAttach: capLocal})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	return sub, lr, "runner:" + rid + "/sandbox"
}

func TestLocalAttachRefusedWithoutTheCapBeforeAnyCall(t *testing.T) {
	sub, lr, ref := localRig(t, false)
	_, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n"})
	if !errors.Is(err, runner.ErrLocalAttachUnsupported) {
		t.Fatalf("LocalAttach on a runner without the cap = %v", err)
	}
	if n := lr.called(); n != 0 {
		t.Fatalf("%d calls reached a runner that did not advertise local attach", n)
	}
}

func TestLocalAttachRoundTripModeAndTeeAck(t *testing.T) {
	sub, lr, ref := localRig(t, true)
	ls, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n", Tee: true, Options: runner.AttachOptions{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	lr.mu.Lock()
	tee := lr.tee
	lr.mu.Unlock()
	if _, err := tee.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := ls.Read(buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("tee read = %q, %v", buf[:n], err)
	}
	if err := ls.SetObserver(ctxT(t), false, 100, 30); err != nil {
		t.Fatal(err)
	}
	if err := ls.AckTee(ctxT(t), 5); err != nil {
		t.Fatal(err)
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if len(lr.modes) != 1 || lr.modes[0].Observer || lr.modes[0].Cols != 100 || lr.modes[0].Stream != tee.ID() {
		t.Fatalf("modes = %+v", lr.modes)
	}
	if len(lr.acks) != 1 || lr.acks[0].Offset != 5 || lr.acks[0].Stream != tee.ID() {
		t.Fatalf("acks = %+v", lr.acks)
	}
}

// An acknowledgement is cumulative: one at or below the last is a no-op that sends nothing.
func TestLocalAttachTeeAckIsMonotonic(t *testing.T) {
	sub, lr, ref := localRig(t, true)
	ls, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n", Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	lr.mu.Lock()
	tee := lr.tee
	lr.mu.Unlock()
	if _, err := tee.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	for read := 0; read < 10; {
		n, err := ls.Read(make([]byte, 10-read))
		if err != nil {
			t.Fatal(err)
		}
		read += n
	}
	for _, off := range []int64{5, 5, 3, 10} {
		if err := ls.AckTee(ctxT(t), off); err != nil {
			t.Fatalf("AckTee(%d) = %v", off, err)
		}
	}
	if err := ls.AckTee(ctxT(t), 11); err == nil {
		t.Fatal("acknowledged a byte never read")
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if len(lr.acks) != 2 || lr.acks[0].Offset != 5 || lr.acks[1].Offset != 10 {
		t.Fatalf("acks sent = %+v, want offsets 5 then 10", lr.acks)
	}
}

// An acknowledgement claims only bytes the org has read, and only for a session that has a tee.
func TestLocalAttachTeeAckRefusals(t *testing.T) {
	sub, lr, ref := localRig(t, true)
	ls, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n", Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	if err := ls.AckTee(ctxT(t), 1); err == nil {
		t.Fatal("acknowledged a byte never read")
	}
	nt, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n"})
	if err != nil {
		t.Fatal(err)
	}
	defer nt.Close()
	if err := nt.AckTee(ctxT(t), 0); err == nil {
		t.Fatal("acknowledged a session that has no tee")
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if len(lr.acks) != 0 {
		t.Fatalf("refused acks reached the runner: %+v", lr.acks)
	}
}

// A mode change the runner does not acknowledge resets the tee, so the runner ends the PTY.
func TestLocalAttachModeFailureResetsTheTee(t *testing.T) {
	sub, lr, ref := localRig(t, true)
	lr.modeErr = runnerwire.Refuse("cannot apply")
	ls, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n", Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.SetObserver(ctxT(t), true, 0, 0); err == nil {
		t.Fatal("a refused mode change succeeded")
	}
	lr.mu.Lock()
	tee := lr.tee
	lr.mu.Unlock()
	var re *runnerwire.ResetError
	if _, err := tee.Read(make([]byte, 1)); !errors.As(err, &re) {
		t.Fatalf("runner end of the tee after a failed mode change = %v, want a reset", err)
	}
}

// local_attach answered with any stream but a pty_tee is refused and closed.
func TestLocalAttachRefusesAReplyThatIsNotATee(t *testing.T) {
	sub, lr, ref := localRig(t, true)
	lr.kind = runnerwire.KindPTY
	if _, err := sub.LocalAttach(ctxT(t), ref, runner.LocalAttachRequest{Nonce: "n"}); err == nil {
		t.Fatal("a pty stream was taken as a local attach tee")
	}
}
