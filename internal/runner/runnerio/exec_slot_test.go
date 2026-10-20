// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// guestSubstrate serves ExecStream from a real guest-agent session, wired as the docker driver's
// vmExecStream wires it, except that the exec context's cancel does not reach the guest request: the
// runner cancels it just before Session.Close, which can free a demux that has not parked yet, so
// without this the hang would depend on timing.
type guestSubstrate struct {
	contextSubstrate
	agent *guestagent.Client
}

func (g guestSubstrate) ExecStream(ctx context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	s, err := g.agent.Exec(context.WithoutCancel(ctx), guestagent.ExecRequest{Argv: spec.Argv, TTY: true})
	if err != nil {
		return nil, err
	}
	return &runner.ExecSession{Stdin: s.Stdin, Stdout: s.Stdout, Stderr: s.Stderr, Resize: func(uint16, uint16) error { return nil }, Wait: s.Wait, Close: s.Close}, nil
}

// A hostile guest streams stdout without end. When the org side drops the stream, the runner stops
// reading, and closing the session must still return and give the exec slot back.
func TestStoppedExecReaderGivesItsSlotBack(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{Protocols: &protocols, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := guestagent.ReadFrame(r.Body); err != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
		flush := w.(http.Flusher).Flush
		flush()
		for guestagent.WriteFrame(w, guestagent.FrameStdout, []byte("hello\n")) == nil {
			flush()
		}
	})}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { _ = hs.Close() })
	agent := guestagent.NewClient(func(context.Context) (net.Conn, error) { return net.Dial("tcp", l.Addr().String()) })
	t.Cleanup(agent.Close)

	server, sub, _, _, ref := streamRigWith(t, func(f *runnertest.Fake) substrate.Substrate {
		return guestSubstrate{contextSubstrate{f}, agent}
	})
	if got := len(server.execSlots); got != 0 {
		t.Fatalf("slots at start = %d", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	e, err := sub.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"yes"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stdout.Read(make([]byte, 6)); err != nil {
		t.Fatal(err)
	}
	// The org resets the stream: the runner's copy fails and stops reading, with the guest still sending.
	if err := e.Stdout.(*runnerwire.Stream).Reset(runnerwire.ResetInternal); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { waitExecsDrained(t, server, "a stopped reader kept its exec slot"); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("exec slot not returned: session Close is blocked on guest output")
	}
}
