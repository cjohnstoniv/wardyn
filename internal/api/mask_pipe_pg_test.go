// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A live session's recording through the batched masker (maskPipe), against the
// Postgres masking registry: two Servers over one database are two wardynds.
// Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func registryMasks(rp replica, run types.AgentRun, value string) bool {
	return strings.Contains(string(rp.reg.Masker(run.ID).Mask([]byte(value))), "<secret-hidden>")
}

// A value registered on A before the bytes are produced is masked in B's
// recording. B runs no background reads and no attach fence here, so the only
// read that can bring the value into B's cache is the one each batch makes.
func TestMaskPipe_PG_AValueRegisteredOnAnotherReplicaIsMasked(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "dispatch-time-value-1")
	tee, finish := b.srv.newSessionRecorder(run, "pipe", runner.AttachOptions{Cols: 80, Rows: 24})
	p := tee.(*maskPipe)
	_, _ = tee.Write([]byte("output before the registration\n"))
	p.flush() // no read in flight across the registration

	const injected = "injected-on-a-before-the-bytes-2"
	if err := a.srv.maskInjected(run.ID, []byte(injected)); err != nil {
		t.Fatal(err)
	}
	if registryMasks(b, run, injected) {
		t.Fatal("B already holds A's value: the test would prove nothing")
	}
	filler := strings.Repeat("o", 4000)
	for i := range 200 {
		line := filler + "\n"
		if i%40 == 3 {
			line = "token " + injected + " " + filler + "\n"
		}
		_, _ = tee.Write([]byte(line))
	}
	// One batch boundary inside the value.
	half := len(injected) / 2
	_, _ = tee.Write([]byte("split " + injected[:half]))
	p.flush()
	_, _ = tee.Write([]byte(injected[half:] + " end\n"))
	finish(t.Context(), types.ActorHuman, maskOwner)

	cast := string(b.rec.saved)
	assertCastHasNoSecret(t, cast, injected, "a value registered on another replica")
	assertCastHasNoSecret(t, cast, "dispatch-time-value-1", "the dispatch value")
	out := decodeCastOutput(t, cast)
	if n := strings.Count(out, "<secret-hidden>"); n != 6 {
		t.Errorf("the recording holds %d placeholders, want 6 (5 whole values and 1 split one)", n)
	}
	if !strings.HasSuffix(out, "split <secret-hidden> end\n") {
		t.Errorf("the split value's line = %q", out[max(0, len(out)-60):])
	}
}

// With B's masking Postgres gone, the batch is replaced by the placeholder: the
// recording never gets bytes B cannot vouch for.
func TestMaskPipe_PG_PostgresDownFailsTheBatchClosed(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	down := newLabPool(t, l)
	b := l.replicaMasking(down)
	run := l.run()
	a.dispatch(t, run, "value-before-the-outage-4")
	tee, finish := b.srv.newSessionRecorder(run, "outage", runner.AttachOptions{Cols: 80, Rows: 24})
	_, _ = tee.Write([]byte("healthy value-before-the-outage-4\n"))
	tee.(*maskPipe).flush()

	down.Close()
	for range 50 {
		_, _ = tee.Write([]byte("during the outage value-before-the-outage-4\n"))
	}
	finish(t.Context(), types.ActorHuman, maskOwner)

	out := decodeCastOutput(t, string(b.rec.saved))
	if !strings.HasPrefix(out, "healthy <secret-hidden>\n") {
		t.Errorf("the healthy line = %q, want it masked", out)
	}
	rest := strings.TrimPrefix(out, "healthy <secret-hidden>\n")
	if rest == "" || strings.ReplaceAll(rest, "<secret-hidden>", "") != "" {
		t.Errorf("with Postgres down the recording got %q, want only placeholders", rest)
	}
}

// 5 MiB through a live attach on B, one 32 KiB exec read per chunk, reaches the
// browser at bandwidth: the pump no longer waits for a registry read (at most one
// per 50ms per replica) per chunk, which held it near 20 chunks a second. Every
// byte arrives, in order, and the recording of the same stream is the stream with
// the value masked, in order.
func TestMaskPipe_PG_AnAttachRelaysAtBandwidthNotAtTheReadRate(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	const value = "dispatch-time-value-5"
	a.dispatch(t, run, value)

	ts := httptest.NewServer(panicFails(t, b.srv.Handler()))
	defer ts.Close()
	tok, err := mintAttachTicket(t.Context(), b.srv.cfg.Store, run.ID, types.ActorHuman, maskOwner, oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	sess := waitForSession(t, b.fr, 0)

	const chunks = 160
	total := chunks * attachReadBuf
	var stream bytes.Buffer
	for i := range chunks {
		chunk := bytes.Repeat([]byte("x"), attachReadBuf)
		copy(chunk, fmt.Sprintf("chunk %06d ", i))
		copy(chunk[100:], value)
		stream.Write(chunk)
	}
	want := stream.Bytes()

	got := make(chan []byte, 1)
	go func() {
		var b bytes.Buffer
		for b.Len() < total {
			typ, data, rerr := c.Read(context.Background())
			if rerr != nil {
				break
			}
			if typ == websocket.MessageBinary {
				b.Write(data)
			}
		}
		got <- b.Bytes()
	}()

	start := time.Now()
	go func() {
		for i := range chunks {
			_, _ = sess.w.Write(want[i*attachReadBuf : (i+1)*attachReadBuf])
		}
	}()
	var relayed []byte
	select {
	case relayed = <-got:
	case <-time.After(60 * time.Second):
		t.Fatal("the stream did not reach the client within 60s")
	}
	elapsed := time.Since(start)
	if !bytes.Equal(relayed, want) {
		t.Fatalf("the client got %d bytes that differ from the %d the exec produced, in order", len(relayed), total)
	}
	rate := float64(chunks) / elapsed.Seconds()
	t.Logf("%d chunks (%d bytes) in %v: %.0f chunks/s, %.1f MB/s", chunks, total, elapsed, rate, float64(total)/elapsed.Seconds()/1e6)
	// Three times the old ceiling, with room for a loaded machine under -race.
	if rate < 60 {
		t.Errorf("%.0f chunks/s: the relay is still bound by the registry read rate", rate)
	}

	_ = c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "the session recording", func() bool { return len(l.recEvents("session.recording.write")) > 0 })
	cast := string(b.rec.saved)
	assertCastHasNoSecret(t, cast, value, "the dispatch value at bandwidth")
	if decodeCastOutput(t, cast) != strings.ReplaceAll(string(want), value, "<secret-hidden>") {
		t.Error("the recording is not the stream with the value masked, in order")
	}
}
