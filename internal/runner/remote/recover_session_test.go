// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// relinkAfterCall is a Link that reconnects once, right after the first call returns: the moment
// between a call's answer and the claim of the stream it names.
type relinkAfterCall struct {
	*Link
	relink func()
	once   sync.Once
}

func (r *relinkAfterCall) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	err := r.Link.Call(ctx, method, args, result, opts...)
	r.once.Do(r.relink)
	return err
}

func (r *relinkAfterCall) pinSession() (session, error) {
	sess, err := r.Link.pinSession()
	if err != nil {
		return nil, err
	}
	return relinkedSession{session: sess, r: r}, nil
}

type relinkedSession struct {
	session
	r *relinkAfterCall
}

func (s relinkedSession) Call(ctx context.Context, method string, args, result any, opts ...runnerwire.CallOption) error {
	err := s.session.Call(ctx, method, args, result, opts...)
	s.r.once.Do(s.r.relink)
	return err
}

// recoverRunner is one runner session that answers every call with a fresh output stream carrying
// payload, after signalling opened. hold, when set, keeps the answer back.
func recoverRunner(ctx context.Context, conn runnerwire.Conn, payload string, opened chan<- struct{}, hold <-chan struct{}) {
	var p *runnerwire.Peer
	p = runnerwire.NewPeer(conn, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, _ string, _ json.RawMessage) (any, error) {
		st, err := p.Open(ctx, runnerwire.Open{Kind: runnerwire.KindOutput, Target: "sb", Call: id})
		if err != nil {
			return nil, err
		}
		go func() { _, _ = io.WriteString(st, payload); _ = st.CloseWrite() }()
		if opened != nil {
			opened <- struct{}{}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}
		return runnerwire.RecoverResult{Stream: st.ID()}, nil
	}})
	go func() { _ = p.Run(ctx) }()
}

// A reconnect between the recovery call's answer and the claim of its stream must not hand the
// caller a stream of the replacement session, whose stream ids start over.
func TestRecoverOutputClaimsOnTheSessionThatAnswered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := make(chan struct{})
	defer close(hold)
	tr := &relinkAfterCall{Link: &Link{}}
	s := New("r1", tr, Options{})
	org1, run1 := runnerwire.Loopback()
	tr.Attach(ctx, org1)
	recoverRunner(ctx, run1, "this run's output", nil, nil)
	tr.relink = func() {
		org2, run2 := runnerwire.Loopback()
		tr.Attach(ctx, org2)
		opened := make(chan struct{}, 1)
		recoverRunner(ctx, run2, "another stream", opened, hold)
		// A call in flight on the new session, whose runner has opened its first stream.
		go func() { _ = tr.Link.Call(ctx, runnerwire.MethodRecoverOutput, runnerwire.RefArgs{Ref: "other"}, nil) }()
		select {
		case <-opened:
		case <-time.After(5 * time.Second):
			t.Error("the new session never opened its stream")
		}
	}
	var out bytes.Buffer
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	err := s.RecoverOutput(rctx, placement.RunnerRefPrefix("r1")+"sb", &out)
	if strings.Contains(out.String(), "another stream") {
		t.Fatalf("recovered %q: a stream of the replacement session", out.String())
	}
	if !errors.Is(err, errOffline) {
		t.Fatalf("RecoverOutput = %v (%q), want the replaced session offline", err, out.String())
	}
}
