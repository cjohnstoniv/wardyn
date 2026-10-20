// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAttachLocalRecordsTheMaskedTee: a granted local session tells the CLI it is local, asks the runner
// for a tee, records the tee through the masker under the session's key, acknowledges what the recorder
// accepted, and writes the session rows with transport local and the runner.
func TestAttachLocalRecordsTheMaskedTee(t *testing.T) {
	const secret = "local-attach-secret-0123456789"
	reg := secretmask.NewRegistry()
	rig := newLocalRig(t, localRigOpts{record: true, mask: reg})
	reg.Add(rig.run.ID, []byte(secret))
	c := rig.dial(t)
	mode := readAttachMode(t, c)
	if mode.Transport != attachTransportLocal || mode.ReadOnly {
		t.Fatalf("first frame = %+v, want a writer on transport local", mode)
	}
	req := rig.sub.req(t, 0)
	if !req.Tee || req.Nonce != "nonce-1" || req.Options.Observer {
		t.Fatalf("local_attach = %+v, want the CLI's nonce, a tee and a writer", req)
	}
	sess := rig.sub.session(t, 0)
	out := []byte("hello " + secret + "\r\n")
	sess.emit(t, out)
	waitFor(t, "the ack of the whole output", func() bool {
		acks := sess.ackLog()
		return len(acks) > 0 && acks[len(acks)-1] == int64(len(out))
	})
	_ = c.Close(websocket.StatusNormalClosure, "")
	if d := rig.detach(t); !strings.Contains(d, `"transport":"local"`) || !strings.Contains(d, `"runner_id":"`+rig.rn.ID.String()+`"`) {
		t.Errorf("session.detach = %s, want transport local and the runner", d)
	}
	for _, ev := range rig.rows("session.attach") {
		if !strings.Contains(string(ev.Data), `"transport":"local"`) {
			t.Errorf("session.attach = %s, want transport local", ev.Data)
		}
	}
	waitFor(t, "the tee to be ended", sess.isClosed)
	waitFor(t, "the recording row", func() bool { return len(rig.rows("session.recording.write")) == 1 })
	var rec struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rig.rows("session.recording.write")[0].Data, &rec)
	rc, err := rig.casts.OpenCast(context.Background(), rec.Key)
	if err != nil {
		t.Fatalf("open the cast %q: %v", rec.Key, err)
	}
	defer rc.Close()
	cast, _ := io.ReadAll(rc)
	assertCastHasNoSecret(t, string(cast), secret, "local attach cast")
	if !strings.Contains(decodeCastOutput(t, string(cast)), "hello ") {
		t.Errorf("cast output %q lacks the session's output", decodeCastOutput(t, string(cast)))
	}
}

// TestAttachLocalAcksOnlyWhatTheRecorderAccepted (M1): an acknowledgement never names a byte the
// recorder has not accepted. The masker is held, so the recorder stops accepting and the runner's
// output backs up behind it; no ack may run ahead of the recorder.
func TestAttachLocalAcksOnlyWhatTheRecorderAccepted(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{record: true, mask: secretmask.NewRegistry()})
	c := rig.dial(t)
	readAttachMode(t, c)
	sess := rig.sub.session(t, 0)
	var holder *attachHolder
	waitFor(t, "the recorded holder", func() bool {
		holder = rig.srv.attachHolderFor(rig.run.ID)
		return holder != nil && holder.cast.Load() != nil
	})
	tee := holder.cast.Load()
	var mu sync.Mutex
	var ahead []string
	sess.mu.Lock()
	sess.onAck = func(offset int64) {
		tee.maskPipe.mu.Lock()
		accepted := tee.maskPipe.queued
		tee.maskPipe.mu.Unlock()
		if offset > accepted {
			mu.Lock()
			ahead = append(ahead, "ack "+itoa(offset)+" > accepted "+itoa(accepted))
			mu.Unlock()
		}
	}
	sess.mu.Unlock()

	tee.mw.mu.Lock() // the recorder stops accepting once its queue is full
	chunk := bytes.Repeat([]byte("x"), attachReadBuf)
	go func() {
		for i := 0; i < 2*(maskPipeMax/attachReadBuf)+4; i++ { // more than the drain's first batch and the queue
			if _, err := sess.pw.Write(chunk); err != nil {
				return
			}
		}
	}()
	waitFor(t, "the recorder to back up", func() bool {
		tee.maskPipe.mu.Lock()
		defer tee.maskPipe.mu.Unlock()
		return len(tee.maskPipe.queue)+attachReadBuf > maskPipeMax
	})
	time.Sleep(100 * time.Millisecond) // any ack the blocked chunk could cause has had its chance
	tee.mw.mu.Unlock()
	mu.Lock()
	got := append([]string(nil), ahead...)
	mu.Unlock()
	if len(got) > 0 {
		t.Fatalf("acks ran ahead of the recorder: %v", got)
	}
	if len(sess.ackLog()) == 0 {
		t.Fatal("no ack was sent for the accepted output")
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	rig.detach(t) // the cast is saved before the row: nothing writes into the temp dir after
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// TestAttachLocalAcksAreBoundedByTheRoundTrip: with a slow ack, steady output sends one ack per round
// trip, never one per chunk, each carrying a newer offset, and the last one the whole output.
func TestAttachLocalAcksAreBoundedByTheRoundTrip(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{record: true, mask: secretmask.NewRegistry()})
	const rtt = 100 * time.Millisecond
	rig.sub.ackDelay = rtt
	c := rig.dial(t)
	readAttachMode(t, c)
	sess := rig.sub.session(t, 0)
	const chunks = 200
	start := time.Now()
	for i := 0; i < chunks; i++ {
		sess.emit(t, []byte("0123456789abcdef"))
	}
	waitFor(t, "the ack of the whole output", func() bool {
		acks := sess.ackLog()
		return len(acks) > 0 && acks[len(acks)-1] == chunks*16
	})
	acks := sess.ackLog()
	if limit := int(time.Since(start)/rtt) + 2; len(acks) > limit || len(acks) >= chunks/4 {
		t.Fatalf("%d acks for %d chunks in %v at a %v round trip: acks are queued per chunk", len(acks), chunks, time.Since(start), rtt)
	}
	for i := 1; i < len(acks); i++ {
		if acks[i] <= acks[i-1] {
			t.Fatalf("acks %v are not each newer than the last", acks)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	rig.detach(t)
}

// TestAttachLocalWithoutARecorderSendsNoAcks: an unrecorded session asks the runner for no tee, and
// bytes a runner sends anyway are dropped: nothing recorded, nothing acknowledged.
func TestAttachLocalWithoutARecorderSendsNoAcks(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{})
	c := rig.dial(t)
	readAttachMode(t, c)
	if rig.sub.req(t, 0).Tee {
		t.Fatal("local_attach asked for a tee with no recorder")
	}
	sess := rig.sub.session(t, 0)
	sess.emit(t, []byte("unrecorded output"))
	sess.emit(t, []byte("more"))
	time.Sleep(50 * time.Millisecond)
	if acks := sess.ackLog(); len(acks) != 0 {
		t.Fatalf("acks %v sent without a recorder", acks)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	rig.detach(t)
	if n := len(rig.rows("session.recording.write")); n != 0 {
		t.Fatalf("%d recording rows without a recorder", n)
	}
}

// TestAttachLocalAckRefusalEndsStalled: an ack the runner refuses (a full call table) is not retried:
// the session ends attach_tee_stalled and the tee is reset.
func TestAttachLocalAckRefusalEndsStalled(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{record: true, mask: secretmask.NewRegistry()})
	rig.sub.ackErr = errors.New("runnerwire: too many calls in flight")
	c := rig.dial(t)
	readAttachMode(t, c)
	done := drainLocal(c)
	sess := rig.sub.session(t, 0)
	sess.emit(t, []byte("output"))
	if d := rig.detach(t); !strings.Contains(d, `"reason":"`+attachTeeStalledReason+`"`) {
		t.Fatalf("session.detach = %s, want reason %s", d, attachTeeStalledReason)
	}
	waitFor(t, "the tee to be reset", sess.isClosed)
	<-done
}

// TestAttachLocalGrantRefusalCloses4409: a runner that does not take the grant ends the socket 4409
// attach_local_grant_failed with one session.detach row, the holder and the slot released.
func TestAttachLocalGrantRefusalCloses4409(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{})
	rig.sub.grantErr = errors.New("unknown or expired nonce")
	c := rig.dial(t)
	_, _, err := c.Read(context.Background())
	if websocket.CloseStatus(err) != attachLocalGrantFailedCode || !strings.Contains(err.Error(), attachLocalGrantFailedReason) {
		t.Fatalf("socket ended with %v, want close %d %s", err, attachLocalGrantFailedCode, attachLocalGrantFailedReason)
	}
	if d := rig.detach(t); !strings.Contains(d, `"reason":"`+attachLocalGrantFailedReason+`"`) || !strings.Contains(d, `"transport":"local"`) {
		t.Fatalf("session.detach = %s, want reason %s on transport local", d, attachLocalGrantFailedReason)
	}
	waitFor(t, "the slot and holder to be released", func() bool {
		return attachSlotsHeld(rig.srv, rig.run.ID) == 0 && rig.srv.attachHolderFor(rig.run.ID) == nil
	})
}

// syncLog is a log sink the server goroutines and the test can share.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// TestAttachLocalNonceNeverLoggedOrAudited: local_nonce is a bearer-like, single-use value for the
// runner alone. A granted session, a refused grant and a request refused at admission, each carrying
// it, leave it in no log line and no audit row (the server logs r.URL.Path, never the query).
func TestAttachLocalNonceNeverLoggedOrAudited(t *testing.T) {
	const nonce = "NONCE-SENTINEL-7f3a9c"
	logs := &syncLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	query := "&transport=local&local_nonce=" + nonce

	granted := newLocalRig(t, localRigOpts{})
	c, resp, err := granted.tryDial(t, granted.ticket(t, holderOwner, oidc.RoleUser), query)
	if err != nil {
		t.Fatalf("dial local attach: %v (%v)", err, resp)
	}
	readAttachMode(t, c)
	if got := granted.sub.req(t, 0).Nonce; got != nonce {
		t.Fatalf("the runner was asked for nonce %q, want the one on the query (the pin would prove nothing)", got)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	granted.detach(t)

	refused := newLocalRig(t, localRigOpts{})
	refused.sub.grantErr = errors.New("unknown or expired nonce")
	c2, _, err := refused.tryDial(t, refused.ticket(t, holderOwner, oidc.RoleUser), query)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = c2.Read(context.Background())
	refused.detach(t)

	denied := newLocalRig(t, localRigOpts{})
	do(t, denied.srv, http.MethodGet, "/api/v1/runs/"+denied.run.ID.String()+"/attach?ticket=not-a-ticket"+query, "", "")

	if strings.Contains(logs.String(), nonce) {
		t.Errorf("the nonce is in a log line:\n%s", logs.String())
	}
	for _, rig := range []*localRig{granted, refused, denied} {
		for _, ev := range rig.audit.snapshot() {
			if b, _ := json.Marshal(ev); strings.Contains(string(b), nonce) {
				t.Errorf("the nonce is in audit row %s: %s", ev.Action, b)
			}
		}
	}
}

// TestAttachLocalTakeOverResetsTheTee: a take-over displaces the local holder, and its runner sees
// pty_tee reset.
func TestAttachLocalTakeOverResetsTheTee(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{})
	c := rig.dial(t)
	readAttachMode(t, c)
	sess := rig.sub.session(t, 0)
	prev, announce := rig.srv.evictAttachHolderFor(rig.run.ID, holderOwner)
	if prev == nil || prev.source != attachSourceLocal {
		t.Fatalf("evicted %+v, want the local holder", prev)
	}
	prev.displace(attachTakeoverReason(holderOwner))
	if announce != nil {
		announce()
	}
	// The client is not reading, so the close handshake cannot finish: the tee is reset at once
	// all the same, not when the socket finally dies.
	waitFor(t, "the tee to be reset", sess.isClosed)
	if err := <-drainLocal(c); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("socket ended with %v, want the take-over close", err)
	}
}

// TestAttachLocalLeaseFencePushesAndEnds: a writer whose lease stops being provable is pushed observer,
// pushed writer again on recovery, and ended "attach lease lost" when the runner does not acknowledge
// a push.
func TestAttachLocalLeaseFencePushesAndEnds(t *testing.T) {
	leases := newLeaseCovLeases()
	rig := newLocalRig(t, localRigOpts{leases: leases})
	c := rig.dial(t)
	if mode := readAttachMode(t, c); mode.ReadOnly {
		t.Fatal("the lease holder was admitted read-only")
	}
	done := drainLocal(c)
	sess := rig.sub.session(t, 0)
	wantModes := func(want ...bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && len(sess.modeLog()) < len(want) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := sess.modeLog(); len(got) != len(want) || got[len(got)-1] != want[len(want)-1] {
			t.Fatalf("pushed modes %v, want %v", got, want)
		}
	}
	leases.fail("holds", errors.New("database stalled"))
	wantModes(true)
	leases.fail("holds", nil)
	wantModes(true, false)

	sess.modeHang.Store(true)
	leases.fail("holds", errors.New("database stalled"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived an unacknowledged push")
	}
	if d := rig.detach(t); !strings.Contains(d, `"reason":"`+attachLeaseLostReason+`"`) {
		t.Fatalf("session.detach = %s, want reason %q", d, attachLeaseLostReason)
	}
	if !sess.isClosed() {
		t.Fatal("the tee was not reset")
	}
}

// TestAttachLocalPromotionReprovesTheCorpus: a local observer promoted when the writer leaves is pushed
// writer only once the run's masking corpus is proved; an unprovable one is not promoted, nothing is
// pushed, and the session ends as a relay promotion does.
func TestAttachLocalPromotionReprovesTheCorpus(t *testing.T) {
	for _, covered := range []bool{true, false} {
		t.Run(map[bool]string{true: "covered", false: "uncovered"}[covered], func(t *testing.T) {
			rig := newLocalRig(t, localRigOpts{})
			// The run loses its corpus proof after the observer is admitted; set before any request,
			// flipped atomically, so no Config is mutated under a live server.
			var lost atomic.Bool
			rig.srv.localCovered = func(uuid.UUID) bool { return !lost.Load() }
			writer := dialAttach(t, rig.ts, rig.srv, rig.run.ID, holderOwner, "")
			readAttachMode(t, writer)
			c := rig.dial(t)
			if mode := readAttachMode(t, c); !mode.ReadOnly || mode.Transport != attachTransportLocal {
				t.Fatalf("second socket = %+v, want a local observer", mode)
			}
			done := drainLocal(c)
			sess := rig.sub.session(t, 0)
			if !rig.sub.req(t, 0).Options.Observer {
				t.Fatal("the observer was granted as a writer")
			}
			lost.Store(!covered)
			_ = writer.Close(websocket.StatusNormalClosure, "")
			if covered {
				waitFor(t, "the writer push", func() bool { m := sess.modeLog(); return len(m) == 1 && !m[0] })
				return
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("an uncovered promotion left the session open")
			}
			if m := sess.modeLog(); len(m) != 0 {
				t.Fatalf("pushed %v to the runner of an uncovered run", m)
			}
			waitFor(t, "the tee to be reset", sess.isClosed)
			waitFor(t, "both detach rows", func() bool { return len(rig.rows("session.detach")) == 2 })
			found := false
			for _, ev := range rig.rows("session.attach") {
				d := string(ev.Data)
				found = found || (ev.Outcome == "failure" && strings.Contains(d, `"promotion":true`) && strings.Contains(d, `"transport":"local"`))
			}
			if !found {
				t.Error("no session.attach failure row for the refused promotion")
			}
		})
	}
}

// TestAttachLocalRevokeEndsWithinOneTick: a sign-out ends an open local session at the next authority
// check, as on the relay, and resets the tee.
func TestAttachLocalRevokeEndsWithinOneTick(t *testing.T) {
	rig := newAuthorityRig(t, 0)
	rn := types.Runner{ID: uuid.New(), Owner: holderOwner, OwnerKind: types.RunnerOwnerPerson, State: types.RunnerClaimed}
	rig.ast.mu.Lock()
	run := rig.ast.runs[rig.run.ID]
	run.OperatorOwned, run.Placement, run.RunnerID = false, types.PlacementLocal, &rn.ID
	rig.ast.runs[run.ID] = run
	rig.ast.mu.Unlock()
	rig.run = run
	rig.srv.cfg.Store = &localAttachStore{profileListStore: &profileListStore{authzStore: rig.ast}, runners: map[uuid.UUID]types.Runner{rn.ID: rn}}
	rig.srv.cfg.AttachLocal = true
	rig.srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
	sub := &fakeLocalSub{caps: runnerwire.Caps{LocalAttach: true}, reported: true}
	rig.srv.localAttachSubs = func(uuid.UUID) localAttachSub { return sub }

	cookie := rig.cookie(t, holderOwner, time.Now().Add(time.Hour))
	url := "ws" + strings.TrimPrefix(rig.ts.URL, "http") + "/api/v1/runs/" + run.ID.String() + "/attach?ticket=" + rig.ticket(t, cookie) + localQuery
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	readAttachMode(t, c)
	done := drainLocal(c)
	sess := sub.session(t, 0)
	if resp := rig.post(t, "/api/v1/auth/logout", cookie); resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-out: %d", resp.StatusCode)
	}
	select {
	case err := <-done:
		if websocket.CloseStatus(err) != attachAuthEndedCode {
			t.Fatalf("socket ended with %v, want %d", err, attachAuthEndedCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the local session outlived the sign-out")
	}
	waitFor(t, "the tee to be reset", sess.isClosed)
	rig.wantDetachReasons(t, 1, attachAuthEndedReason)
}

// TestRunnerLocalAttachOnlyForAPersonsRunnerWithTheFlag: AUTH_OK says local_attach only with the flag on
// and only to a person's runner, never an organisation's (C4).
func TestRunnerLocalAttachOnlyForAPersonsRunnerWithTheFlag(t *testing.T) {
	for _, c := range []struct {
		on   bool
		kind types.RunnerOwnerKind
		want bool
	}{
		{true, types.RunnerOwnerPerson, true},
		{false, types.RunnerOwnerPerson, false},
		{true, types.RunnerOwnerOrg, false},
	} {
		s := &Server{cfg: Config{AttachLocal: c.on}}
		if got := s.runnerLocalAttach(types.Runner{OwnerKind: c.kind}); got != c.want {
			t.Errorf("flag %v, %s runner: local_attach = %v, want %v", c.on, c.kind, got, c.want)
		}
	}
}
