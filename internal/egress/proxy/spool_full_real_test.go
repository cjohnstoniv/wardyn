// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

// The spool reports itself full to a proxy that real requests cannot be admitted by. A request
// reserves spoolReservationBytes (the record envelope plus 12 bytes per byte of its target), so the
// proxy stalls a few hundred bytes above the smallest envelope, not at it. This drives the real
// reservation of real targets through the real remote sink into a real spool until the proxy is
// refused, and asks the spool whether it is full: runnerd ends a VM on that answer.
func TestSpoolIsFullOnceRealRequestsAreRefused(t *testing.T) {
	ctx := t.Context()
	rs, f := startedSink(t)
	dir := filepath.Join(t.TempDir(), "spool") // Open makes it private
	sp, err := runnerspool.Open(dir, rs.runID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	sess, err := sp.Accept(ctx, rs.instance)
	if err != nil {
		t.Fatal(err)
	}
	c := accept(t, rs, f.next(), sess.Gen, sess.Seq, sess.Grant.Through, sess.Grant.Bytes)

	// Leave the spool 100 bytes more credit than the smallest envelope: a stall the old
	// "room below the smallest envelope" test never sees.
	const roomLeft = runnerspool.MinRequestReservation + 100
	state := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["last"] = 10
	m["bytes"] = runnerspool.MaxBytes - runnerspool.LossReserveBytes - runnerspool.GrantRecords*512 - roomLeft
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	g, changed, err := sp.Regrant(ctx, sess.Gen, rs.instance)
	if err != nil || !changed {
		t.Fatalf("regrant = %+v, %v, %v", g, changed, err)
	}
	writeFrame(t, c, SinkFrame{T: FrameGrant, Gen: sess.Gen, Base: g.Base, Through: g.Through, Bytes: g.Bytes})
	rs.mu.Lock()
	for rs.bytes != g.Bytes {
		rs.cond.Wait()
	}
	rs.mu.Unlock()
	if full, err := sp.Full(ctx); err != nil || full {
		t.Fatalf("Full before any request was refused = %v, %v", full, err)
	}

	p := &Proxy{}
	var refused uint64
	for _, target := range []string{
		"http://a/",
		"https://api.anthropic.com/v1/messages",
		"https://registry.npmjs.org/typescript/-/typescript-5.4.5.tgz",
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		need := p.spoolReservationBytes(req)
		if need <= g.Bytes || need <= runnerspool.MinRequestReservation {
			t.Fatalf("%s reserves %d against %d bytes of credit: not the stall this pins", target, need, g.Bytes)
		}
		if _, err := rs.reserve(short(t), need); !errors.Is(err, runnerspool.ErrFull) {
			t.Fatalf("%s reserve = %v, want the full-spool refusal", target, err)
		}
		refused++
		if r := readUntil(t, c, FrameRefused); r.Total != refused {
			t.Fatalf("refused frame %+v, want total %d", r, refused)
		}
		if err := sp.ProducerRefused(ctx, sess.Gen, rs.instance, refused); err != nil {
			t.Fatal(err)
		}
		if full, err := sp.Full(ctx); err != nil || !full {
			t.Fatalf("Full after %s was refused = %v, %v; want true", target, full, err)
		}
	}
}
