// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// IN-PLACE PROMOTION: a read-only observer becomes the writer on the SOCKET IT
// ALREADY HAS when the writer leaves.
//
// The failure these pin is not "no promotion frame arrives" — a registry that
// announces a promotion it never performed sends one just fine. It is the
// CAPABILITY: writable actually flips, so the promoted client's keystrokes
// reach the sandbox and its own detach row says it was driving. Force
// promoted.writable false (attach_holder.go, both promotion sites) and every
// test in this file fails; assert only "a second frame arrived" and none of
// them would.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// readNextAttachMode reads the NEXT attach-mode control frame on an
// already-open socket — the promotion. PTY output may interleave, so binary
// frames are skipped rather than treated as a protocol error.
func readNextAttachMode(t *testing.T, c *websocket.Conn) attachModeMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read the promotion frame: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg attachModeMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode control frame %q: %v", data, err)
		}
		if msg.Type == "attach-mode" {
			return msg
		}
	}
}

// waitForActorAudit is waitForAudit narrowed to ONE principal: a promotion
// leaves two session.detach rows on the same run, and "the departing writer
// was driving" and "the promoted observer was driving" are different claims.
func waitForActorAudit(t *testing.T, rec *sshTestRecorder, runID uuid.UUID, action, actor string) *types.AuditEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		events := rec.snapshot()
		for i := range events {
			ev := &events[i]
			if ev.Action == action && ev.Actor == actor && ev.RunID != nil && *ev.RunID == runID {
				return ev
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit for %s; events = %s", action, actor, auditDump(events, runID))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAttachPromotion_RegistryQueueIsFIFO is the registry contract under the
// two transports: observers queue in arrival order, the writer's release
// promotes the OLDEST one still on its socket, and the flip happens under the
// registry lock while the notice is handed back to be run outside it.
func TestAttachPromotion_RegistryQueueIsFIFO(t *testing.T) {
	rec := &sshTestRecorder{}
	srv := New(Config{Audit: rec, AdminToken: adminToken})
	runID := uuid.New()

	notified := make(chan *attachHolder, 4)
	client := func(principal string) *attachHolder {
		h := &attachHolder{principal: principal, actorType: types.ActorHuman, source: attachSourceWeb, since: time.Now()}
		h.notify = func(readOnly bool, got *attachHolder) {
			if readOnly {
				t.Errorf("%s was notified with read_only=true; promotion is the only mode change there is", principal)
			}
			notified <- got
		}
		return h
	}

	writer, first, second, gone := client("writer@x"), client("first@x"), client("second@x"), client("gone@x")
	readOnly, releaseWriter := srv.registerAttachHolder(runID, writer)
	if readOnly {
		t.Fatal("the first client was admitted read-only; nobody held the PTY")
	}
	releases := map[*attachHolder]func() func(){}
	for _, h := range []*attachHolder{first, gone, second} {
		ro, release := srv.registerAttachHolder(runID, h)
		if !ro {
			t.Fatalf("%s was admitted writable behind a live holder", h.principal)
		}
		if h.canWrite() {
			t.Fatalf("observer %s may write", h.principal)
		}
		releases[h] = release
	}
	if !writer.canWrite() {
		t.Fatal("the registered writer may not write")
	}

	// An observer that leaves the queue is not promoted from beyond the grave.
	if announce := releases[gone](); announce != nil {
		t.Fatal("an OBSERVER's release announced a promotion; only the writer's does")
	}

	announce := releaseWriter()
	if announce == nil {
		t.Fatal("the writer left with two observers queued and nothing was promoted")
	}
	// The flip is already done — under the registry lock, not in the notice.
	if got := srv.attachHolderFor(runID); got != first {
		t.Fatalf("attachHolderFor = %+v, want the oldest observer %s", got, first.principal)
	}
	if !first.canWrite() {
		t.Error("the promoted observer still cannot write — the registry announced a promotion it did not perform")
	}
	if second.canWrite() {
		t.Error("a bystander observer was promoted too; one writer at a time is the whole rule")
	}

	announce()
	select {
	case got := <-notified:
		if got != first {
			t.Fatalf("notified %+v, want %s", got, first.principal)
		}
	default:
		t.Fatal("the promoted client was never told it may type")
	}
	ev := waitForActorAudit(t, rec, runID, "session.promote", first.principal)
	if !strings.Contains(string(ev.Data), `"previous_holder":"writer@x"`) {
		t.Errorf("session.promote data = %s, want previous_holder writer@x", ev.Data)
	}

	// The chain continues: the promoted client's own release promotes the next.
	if announce := releases[first](); announce == nil {
		t.Fatal("the promoted client's release promoted nobody")
	} else {
		announce()
	}
	if got := srv.attachHolderFor(runID); got != second {
		t.Fatalf("attachHolderFor = %+v, want %s", got, second.principal)
	}
	if announce := releases[second](); announce != nil {
		t.Fatal("the last client's release promoted somebody out of an empty queue")
	}
	if got := srv.attachHolderFor(runID); got != nil {
		t.Fatalf("phantom holder after everyone left: %+v", got)
	}
}

// TestAttachPromotion_WebObserverPromotedInPlace is the issue's acceptance over
// a REAL WebSocket: the second dial reads read_only:true, the writer's ordinary
// close (not a take-over) delivers a SECOND attach-mode frame on that same
// socket naming the observer itself as holder, a keystroke after it reaches the
// sandbox, and the detach row records what the client ENDED as.
func TestAttachPromotion_WebObserverPromotedInPlace(t *testing.T) {
	srv, _, fr, audit, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	if m := readAttachMode(t, c1); m.ReadOnly {
		t.Fatal("the first client was told it is read-only")
	}
	waitFor(t, "the writer to register", func() bool { return srv.attachHolderFor(run.ID) != nil })

	c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "")
	m2 := readAttachMode(t, c2)
	if !m2.ReadOnly {
		t.Fatal("the second client was admitted writable; two clients would fight over one tmux session")
	}
	if m2.Holder == nil || m2.Holder.Principal != holderOwner {
		t.Fatalf("the observer was not told who holds the PTY: %+v", m2.Holder)
	}
	waitFor(t, "the observer's session to open", func() bool { return fr.session(1) != nil })
	observed := fr.session(1)

	// The writer leaves the ordinary way — a clean close, nobody displaced.
	if err := c1.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the writer's socket: %v", err)
	}

	m3 := readNextAttachMode(t, c2)
	if m3.ReadOnly {
		t.Fatal("the observer was told read_only:true again; it was never promoted")
	}
	if m3.Holder == nil || m3.Holder.Principal != holderSecond {
		t.Fatalf("promotion frame holder = %+v, want the promoted client %s itself", m3.Holder, holderSecond)
	}
	if got := srv.attachHolderFor(run.ID); got == nil || got.principal != holderSecond {
		t.Fatalf("attachHolderFor = %+v, want the promoted client %s", got, holderSecond)
	}

	// The capability, not the announcement: its keystrokes now reach the PTY on
	// the socket it already had — no reconnect anywhere in this test.
	wsWrite(t, c2, websocket.MessageBinary, []byte("echo promoted\r"))
	waitFor(t, "the promoted client's keystrokes to reach the sandbox", func() bool { return observed.written() > 0 })

	promo := waitForActorAudit(t, audit, run.ID, "session.promote", holderSecond)
	for _, want := range []string{`"previous_holder":"` + holderOwner + `"`, `"principal":"` + holderSecond + `"`, `"source":"web"`} {
		if !strings.Contains(string(promo.Data), want) {
			t.Errorf("session.promote data = %s, want %s", promo.Data, want)
		}
	}

	// The departing writer detaches as a writer...
	if ev := waitForActorAudit(t, audit, run.ID, "session.detach", holderOwner); !strings.Contains(string(ev.Data), `"read_only":false`) {
		t.Errorf("the writer's session.detach = %s, want read_only:false", ev.Data)
	}
	// ...and so does the client that arrived as an observer, because by the time
	// it left it was the one driving.
	if err := c2.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the promoted socket: %v", err)
	}
	if ev := waitForActorAudit(t, audit, run.ID, "session.detach", holderSecond); !strings.Contains(string(ev.Data), `"read_only":false`) {
		t.Errorf("the promoted client's session.detach = %s, want read_only:false — the row must report the LIVE mode, not the connect-time one", ev.Data)
	}
}

// TestAttachPromotion_TakeoverPromotesOnlyTheTaker pins both halves of the
// take-over rule. A take-over is an audited act attributed to ONE principal, so
// it may only hand the terminal to that principal's own socket; promoting the
// oldest bystander instead would attribute a stranger's typing to the taker.
func TestAttachPromotion_TakeoverPromotesOnlyTheTaker(t *testing.T) {
	t.Run("the taker's own observer is promoted in place", func(t *testing.T) {
		srv, _, fr, audit, run := holderTestServer(t)
		ts := httptest.NewServer(panicFails(t, srv.Handler()))
		defer ts.Close()
		owner := ssoSession(t, holderOwner, holderOwner, oidc.RoleMember)

		// Somebody else is driving the run's owner's terminal...
		c1 := dialAttach(t, ts, srv, run.ID, holderSecond, "")
		if m := readAttachMode(t, c1); m.ReadOnly {
			t.Fatal("the first client was told it is read-only")
		}
		// ...while the owner watches from a read-only socket.
		c2 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
		if m := readAttachMode(t, c2); !m.ReadOnly {
			t.Fatal("the second client was admitted writable")
		}
		waitFor(t, "the observer's session to open", func() bool { return fr.session(1) != nil })
		observed := fr.session(1)

		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", owner, "")
		if w.Code != http.StatusOK {
			t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
		}
		// The UI's doTakeover (attach-terminal.tsx) branches on this field: it
		// must NOT evict-then-reconnect its own socket when the server already
		// promoted it in place, or it closes the very socket just promoted and
		// hands the writer slot to the next queued observer by FIFO (#507).
		var takeoverBody struct {
			Promoted bool `json:"promoted"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &takeoverBody); err != nil {
			t.Fatalf("decode takeover body %q: %v", w.Body.String(), err)
		}
		if !takeoverBody.Promoted {
			t.Fatalf("takeover body promoted = %v, want true (the taker had a queued observer socket)", takeoverBody.Promoted)
		}

		m := readNextAttachMode(t, c2)
		if m.ReadOnly || m.Holder == nil || m.Holder.Principal != holderOwner {
			t.Fatalf("the taker's own socket was not promoted in place: read_only=%v holder=%+v", m.ReadOnly, m.Holder)
		}
		wsWrite(t, c2, websocket.MessageBinary, []byte("mine now\r"))
		waitFor(t, "the taker's keystrokes to reach the sandbox", func() bool { return observed.written() > 0 })
		if ev := waitForActorAudit(t, audit, run.ID, "session.promote", holderOwner); !strings.Contains(string(ev.Data), `"previous_holder":"`+holderSecond+`"`) {
			t.Errorf("session.promote data = %s, want previous_holder %s", ev.Data, holderSecond)
		}

		// The displaced client still learns why, on the close frame the UI
		// matches: promotion did not cost it its reason.
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		if _, _, err := c1.Read(rctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Fatalf("displaced close status = %v, want 1008", websocket.CloseStatus(err))
		}
	})

	t.Run("a bystander is never promoted", func(t *testing.T) {
		srv, _, fr, audit, run := holderTestServer(t)
		ts := httptest.NewServer(panicFails(t, srv.Handler()))
		defer ts.Close()
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

		c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
		if m := readAttachMode(t, c1); m.ReadOnly {
			t.Fatal("the first client was told it is read-only")
		}
		c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "")
		if m := readAttachMode(t, c2); !m.ReadOnly {
			t.Fatal("the second client was admitted writable")
		}
		waitFor(t, "the observer's session to open", func() bool { return fr.session(1) != nil })
		observed := fr.session(1)
		go drainClient(c2)

		// An admin with no socket of its own takes over: the slot goes FREE.
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
		if w.Code != http.StatusOK {
			t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
		}
		if got := srv.attachHolderFor(run.ID); got != nil {
			t.Fatalf("attachHolderFor = %+v after a take-over by a principal with no observer, want nobody", got)
		}
		// The bystander stays read-only. Barrier, not a sleep: the server
		// answers the ping from the very read loop that consumed the keystroke.
		wsWrite(t, c2, websocket.MessageBinary, []byte("not mine\r"))
		wsPing(t, c2)
		if n := observed.written(); n != 0 {
			t.Errorf("the bystander's keystrokes reached the sandbox (%d writes) — it was promoted by somebody else's take-over", n)
		}
		if ev := findAudit(audit.snapshot(), run.ID, "session.promote", "success"); ev != nil {
			t.Errorf("a take-over with no observer of its own audited a promotion: %s", ev.Data)
		}
	})
}

// TestAttachPromotion_SSHObserverPromoted: the CLI lane is the same registry,
// so a `wardyn attach` watching over the browser's shoulder inherits the
// terminal the same way — and, unlike the browser, must re-apply its own
// window, because it never resized the shared tmux session while it watched.
func TestAttachPromotion_SSHObserverPromoted(t *testing.T) {
	srv, _, fr, audit, run := holderTestServer(t)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	if m := readAttachMode(t, c1); m.ReadOnly {
		t.Fatal("the web client was told it is read-only")
	}
	waitFor(t, "the web writer to register", func() bool { return srv.attachHolderFor(run.ID) != nil })

	ch := newFakeSSHChannel()
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.bridgeSSHShell(context.Background(), run.ID, holderSecond, ch, 120, 40, resizeCh)
	}()
	waitFor(t, "the read-only notice on the ssh channel", func() bool {
		return strings.Contains(ch.stderrString(), "read-only")
	})
	waitFor(t, "the ssh observer's session to open", func() bool { return fr.session(1) != nil })
	observed := fr.session(1)
	if n := observed.resizeCount(); n != 0 {
		t.Fatalf("the ssh observer resized the shared tmux window (%d resizes) before it held anything", n)
	}

	if err := c1.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the web writer's socket: %v", err)
	}

	waitFor(t, "the promoted ssh client to be told", func() bool {
		return strings.Contains(ch.stderrString(), "you now hold this terminal")
	})
	waitFor(t, "the promoted ssh client's own geometry to be applied", func() bool { return observed.resizeCount() > 0 })
	if v := srv.attachHolderFor(run.ID).view(); !v.Held || v.Principal != holderSecond || v.Source != attachSourceSSH || v.Cols != 120 {
		t.Fatalf("attach-holder = %+v, want the promoted ssh client %s at 120 cols", v, holderSecond)
	}

	ch.clientSend("echo promoted\n")
	waitFor(t, "the promoted ssh client's keystrokes to reach the sandbox", func() bool { return observed.written() > 0 })

	_ = ch.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the promoted ssh bridge never returned")
	}
	if ev := waitForActorAudit(t, audit, run.ID, "session.detach", holderSecond); !strings.Contains(string(ev.Data), `"read_only":false`) {
		t.Errorf("the promoted ssh client's session.detach = %s, want read_only:false", ev.Data)
	}
}
