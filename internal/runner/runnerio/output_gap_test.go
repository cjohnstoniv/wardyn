// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// gapCapture is a recovery writer that can mark a gap (runner.OutputGapMarker).
type gapCapture struct {
	bytes.Buffer
	skipped []int64
}

func (c *gapCapture) OutputGap(skipped int64) { c.skipped = append(c.skipped, skipped) }

// A runner that already discarded acknowledged output gives an org that can mark the gap the rest of
// the copy after it; an org that cannot gets unrecoverable, as before.
func TestRecoverOutputAfterDiscardedBytesCarriesTheGap(t *testing.T) {
	fake := runnertest.NewFake()
	server, err := New(fake, "laptop", lateWriterDir(t))
	if err != nil {
		t.Fatal(err)
	}
	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	caps, _ := json.Marshal(runnerwire.Caps{Support: fake.Support})
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
		w, err := server.Output(args.Spec.RunID)
		if err != nil {
			return nil, err
		}
		args.Spec.ExecOutput = w
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	capture := &outputCapture{done: make(chan struct{})}
	id := uuid.New()
	sb, err := sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: id, ExecOutput: capture})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-capture.done:
	case <-ctx.Done():
		t.Fatal("output never completed")
	}
	// The acknowledged bytes are discarded, as a full buffer does.
	server.mu.Lock()
	b := server.outputs[id]
	server.mu.Unlock()
	b.mu.Lock()
	err = b.evict(false)
	start := b.start()
	b.mu.Unlock()
	if err != nil || start == 0 {
		t.Fatalf("evict: start %d, %v", start, err)
	}

	var plain bytes.Buffer
	if err := sub.RecoverOutput(ctx, sb.Ref, &plain); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("a writer that cannot mark a gap: %v, want unrecoverable", err)
	}
	var gapped gapCapture
	if err := sub.RecoverOutput(ctx, sb.Ref, &gapped); err != nil {
		t.Fatalf("a writer that marks the gap: %v", err)
	}
	if len(gapped.skipped) != 1 || gapped.skipped[0] != start || gapped.Len() != 0 {
		t.Fatalf("gap %v and %q, want one gap of %d and nothing after it", gapped.skipped, gapped.String(), start)
	}
}
