// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
)

// realSpool is a real remote sink on a real spool whose grant holds exactly roomLeft bytes of credit:
// the spool is left that near its byte ceiling and regranted, the way a long offline stretch leaves it.
type realSpool struct {
	rs   *remoteSink
	sp   *runnerspool.Spool
	c    net.Conn
	gen  uint64
	room int64
}

func newRealSpool(t *testing.T, roomLeft int64) *realSpool {
	t.Helper()
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
	if err != nil || !changed || g.Bytes != roomLeft {
		t.Fatalf("regrant = %+v, %v, %v; want %d bytes of credit", g, changed, err, roomLeft)
	}
	writeFrame(t, c, SinkFrame{T: FrameGrant, Gen: sess.Gen, Base: g.Base, Through: g.Through, Bytes: g.Bytes})
	rs.mu.Lock()
	for rs.bytes != g.Bytes {
		rs.cond.Wait()
	}
	rs.mu.Unlock()
	return &realSpool{rs: rs, sp: sp, c: c, gen: sess.Gen, room: roomLeft}
}

func (r *realSpool) full(t *testing.T) bool {
	t.Helper()
	full, err := r.sp.Full(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return full
}

// refuse asks to reserve need and expects the counted full-spool refusal, which it carries to the spool
// the way runnerd's sink does.
func (r *realSpool) refuse(t *testing.T, need int64, total uint64) {
	t.Helper()
	if _, err := r.rs.reserve(short(t), need); !errors.Is(err, runnerspool.ErrFull) {
		t.Fatalf("reserve(%d) against %d bytes of credit = %v, want the full-spool refusal", need, r.room, err)
	}
	if f := readUntil(t, r.c, FrameRefused); f.Total != total {
		t.Fatalf("refused frame %+v, want total %d", f, total)
	}
	if err := r.sp.ProducerRefused(t.Context(), r.gen, r.rs.instance, total); err != nil {
		t.Fatal(err)
	}
}

// The spool reports itself full to a proxy that real requests cannot be admitted by. A request
// reserves spoolReservationBytes (the record envelope plus 12 bytes per byte of its target), so the
// proxy stalls a few hundred bytes above the smallest envelope, not at it. This drives the real
// reservation of real targets through the real remote sink into a real spool until the proxy is
// refused, and asks the spool whether it is full: runnerd ends a VM on that answer.
func TestSpoolIsFullOnceRealRequestsAreRefused(t *testing.T) {
	// Leave the spool 100 bytes more credit than the smallest envelope: a stall the old
	// "room below the smallest envelope" test never sees.
	r := newRealSpool(t, runnerspool.MinRequestReservation+100)
	if r.full(t) {
		t.Fatal("Full before any request was refused")
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
		if need <= r.room || need <= runnerspool.MinRequestReservation {
			t.Fatalf("%s reserves %d against %d bytes of credit: not the stall this pins", target, need, r.room)
		}
		refused++
		r.refuse(t, need, refused)
		if !r.full(t) {
			t.Fatalf("Full after %s was refused = false; want true", target)
		}
	}
}

// With content scanning on, a POST reserves a whole record (MaxRecordBytes), so the proxy stalls up to a
// megabyte above the smallest envelope. The spool is full once such a POST is refused with less than a
// record of credit left, and is not while the POST is admitted (credit of exactly MaxRecordBytes).
func TestSpoolIsFullOnceAScannedPostIsRefused(t *testing.T) {
	post, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{scanner: scanEngine(t, "alert", scanTestSecret)}
	need := p.spoolReservationBytes(post)
	if need != runnerspool.MaxRecordBytes {
		t.Fatalf("a scanned POST reserves %d, want MaxRecordBytes %d", need, runnerspool.MaxRecordBytes)
	}
	for _, tc := range []struct {
		name     string
		room     int64
		admitted bool
	}{
		{"one byte short of a record", runnerspool.MaxRecordBytes - 1, false},
		{"exactly a record", runnerspool.MaxRecordBytes, true},
		{"half a megabyte", 512 << 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRealSpool(t, tc.room)
			if r.full(t) {
				t.Fatal("Full before any request was refused")
			}
			if tc.admitted {
				if _, err := r.rs.reserve(t.Context(), need); err != nil {
					t.Fatalf("a POST needing %d bytes against %d: %v", need, tc.room, err)
				}
				if r.full(t) {
					t.Fatal("Full although the POST was admitted")
				}
				return
			}
			r.refuse(t, need, 1)
			if !r.full(t) {
				t.Fatalf("Full after the POST was refused with %d bytes of credit = false; want true", tc.room)
			}
		})
	}
}
