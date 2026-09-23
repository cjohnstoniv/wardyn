// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// CROSS-TRANSPORT + RECONNECT: attach_promotion_test.go already pins in-place
// promotion over a single transport (two web clients, an SSH observer behind a
// web writer, a web-only take-over). This file adds the cases that mix
// transports and the case that crosses a daemon restart:
//
//   - a web observer is promoted while an SSH holder is displaced by a
//     take-over, and only when the observer is the TAKER's own socket;
//   - reconnecting after the daemon process is rebuilt against the same store.
//
// Same failure this whole corpus pins: a registry that announces a promotion
// it never performs. Force promoted.writable false at BOTH promotion sites in
// attach_holder.go (the release closure in registerAttachHolder, and
// evictAttachHolderFor) and every test below fails — never just "no second
// frame arrived".

import (
	"context"
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

// TestAttachCorpus_TakeoverPromotesWebObserverOverSSHHolder is the cross-
// transport twin of TestAttachPromotion_TakeoverPromotesOnlyTheTaker: there the
// displaced holder and both observers were web sockets, so it never exercised
// evictAttachHolderFor against a holder registered over bridgeSSHShell. The
// rule is transport-blind — the taker's OWN observer is promoted in place, a
// bystander never is — and that must hold when the socket being displaced is
// the SSH one and the socket being promoted is a web one.
func TestAttachCorpus_TakeoverPromotesWebObserverOverSSHHolder(t *testing.T) {
	t.Run("the taker's own web observer is promoted in place", func(t *testing.T) {
		srv, _, fr, audit, run := holderTestServer(t)
		ts := httptest.NewServer(panicFails(t, srv.Handler()))
		defer ts.Close()
		owner := ssoSession(t, holderOwner, holderOwner, oidc.RoleMember)

		// holderSecond holds the run's terminal from the CLI...
		ch := newFakeSSHChannel()
		resizeCh := make(chan sshWindowChangeMsg, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.bridgeSSHShell(context.Background(), run.ID, holderSecond, ch, 80, 24, resizeCh)
		}()
		waitFor(t, "the ssh holder to register", func() bool { return srv.attachHolderFor(run.ID) != nil })

		// ...while the run's OWNER watches from a read-only browser tab.
		c2 := dialAttach(t, ts, srv, run.ID, holderOwner, "")
		m2 := readAttachMode(t, c2)
		if !m2.ReadOnly || m2.Holder == nil || m2.Holder.Principal != holderSecond || m2.Holder.Source != attachSourceSSH {
			t.Fatalf("the web observer's attach-mode = %+v, want read-only behind the ssh holder %s", m2, holderSecond)
		}
		waitFor(t, "the web observer's session to open", func() bool { return fr.session(1) != nil })
		observed := fr.session(1)

		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", owner, "")
		if w.Code != http.StatusOK {
			t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
		}

		// The taker's own (web) socket is promoted IN PLACE — no reconnect.
		m3 := readNextAttachMode(t, c2)
		if m3.ReadOnly || m3.Holder == nil || m3.Holder.Principal != holderOwner || m3.Holder.Source != attachSourceWeb {
			t.Fatalf("the taker's own web observer was not promoted in place: %+v", m3)
		}
		if got := srv.attachHolderFor(run.ID); got == nil || got.principal != holderOwner || got.source != attachSourceWeb {
			t.Fatalf("attachHolderFor = %+v, want the promoted web client %s", got, holderOwner)
		}

		// The capability, not just the announcement: its keystrokes now reach the
		// sandbox on the socket it already had.
		wsWrite(t, c2, websocket.MessageBinary, []byte("mine now\r"))
		waitFor(t, "the promoted web client's keystrokes to reach the sandbox", func() bool { return observed.written() > 0 })

		// The displaced SSH holder learns why, on its own transport's equivalent
		// of the close frame, and the bridge returns.
		waitFor(t, "the displaced ssh client to be told why", func() bool {
			return strings.Contains(ch.stderrString(), attachTakeoverReasonPrefix)
		})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the displaced ssh bridge never returned")
		}

		promo := waitForActorAudit(t, audit, run.ID, "session.promote", holderOwner)
		for _, want := range []string{`"previous_holder":"` + holderSecond + `"`, `"principal":"` + holderOwner + `"`, `"source":"web"`} {
			if !strings.Contains(string(promo.Data), want) {
				t.Errorf("session.promote data = %s, want %s", promo.Data, want)
			}
		}
	})

	t.Run("a foreign web observer is never promoted", func(t *testing.T) {
		srv, _, fr, audit, run := holderTestServer(t)
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

		// The run's owner holds it from the CLI...
		ch := newFakeSSHChannel()
		resizeCh := make(chan sshWindowChangeMsg, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch, 80, 24, resizeCh)
		}()
		waitFor(t, "the ssh holder to register", func() bool { return srv.attachHolderFor(run.ID) != nil })

		// ...while a bystander watches from the browser, and an ADMIN with no
		// observer socket of its own does the taking over.
		ts := httptest.NewServer(panicFails(t, srv.Handler()))
		defer ts.Close()
		c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "")
		if m := readAttachMode(t, c2); !m.ReadOnly {
			t.Fatal("the bystander was admitted writable")
		}
		waitFor(t, "the bystander's session to open", func() bool { return fr.session(1) != nil })
		observed := fr.session(1)
		go drainClient(c2)

		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
		if w.Code != http.StatusOK {
			t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
		}
		waitFor(t, "the displaced ssh client to be told why", func() bool {
			return strings.Contains(ch.stderrString(), attachTakeoverReasonPrefix)
		})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the displaced ssh bridge never returned")
		}

		// The slot is FREE — nobody's own socket was there to promote.
		if got := srv.attachHolderFor(run.ID); got != nil {
			t.Fatalf("attachHolderFor = %+v after a take-over by a principal with no observer, want nobody", got)
		}
		// The bystander stays read-only. Barrier, not a sleep: the server answers
		// the ping from the very read loop that consumed the keystroke.
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

// TestAttachCorpus_ReconnectAfterDaemonRebuildSharesStore: the attach-holder
// registry is explicitly IN-PROCESS (attach_holder.go's doc on
// attachHolderRegistry) — a daemon restart wipes it while the run itself lives
// on in the store. A rebuilt daemon must not inherit a phantom holder from the
// process that crashed, and a client that reconnects afterward must go through
// the exact same promotion path as one that had been attached the whole time:
// a second, later client queues as an observer and is promoted in place when
// the reconnected writer leaves.
func TestAttachCorpus_ReconnectAfterDaemonRebuildSharesStore(t *testing.T) {
	h := newHarness(t)
	ast := newAuthzStore()
	st := &touchCountingStore{authzStore: ast}
	rec := &sshTestRecorder{} // shared audit sink: a real one survives a restart too
	run := types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, State: types.RunRunning, SandboxRef: "sbx-reconnect"}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()

	build := func(fr *holderTestRunner) *Server {
		cfg := baseTestConfig(h, st)
		cfg.Audit = rec
		cfg.Runner = fr
		cfg.OIDC = &oidc.Authenticator{}
		return New(cfg)
	}

	// the first daemon process: a writer attaches and registers
	fr1 := &holderTestRunner{}
	srv1 := build(fr1)
	ts1 := httptest.NewServer(panicFails(t, srv1.Handler()))

	c1 := dialAttach(t, ts1, srv1, run.ID, holderOwner, "&cols=80&rows=24")
	if m := readAttachMode(t, c1); m.ReadOnly {
		t.Fatal("the writer was told it is read-only")
	}
	waitFor(t, "the writer to register on the first daemon", func() bool { return srv1.attachHolderFor(run.ID) != nil })

	// The process crashes: no clean close on either the socket or the
	// registry, exactly what an in-process map cannot survive.
	ts1.Close()

	// the daemon restarts: a new Server, the same store, a fresh registry
	fr2 := &holderTestRunner{}
	srv2 := build(fr2)
	ts2 := httptest.NewServer(panicFails(t, srv2.Handler()))
	defer ts2.Close()

	if got := srv2.attachHolderFor(run.ID); got != nil {
		t.Fatalf("a rebuilt daemon inherited a phantom holder from the crashed process: %+v", got)
	}

	// Reconnect: the SAME principal is admitted WRITER again — not stuck
	// read-only behind a holder that died with the old process.
	c2 := dialAttach(t, ts2, srv2, run.ID, holderOwner, "&cols=80&rows=24")
	if m := readAttachMode(t, c2); m.ReadOnly {
		t.Fatal("the reconnecting client was admitted read-only after a daemon rebuild")
	}
	waitFor(t, "the writer to register on the rebuilt daemon", func() bool { return srv2.attachHolderFor(run.ID) != nil })

	// A second, later client observes against the SAME run, on the SAME store,
	// through the rebuilt daemon.
	c3 := dialAttach(t, ts2, srv2, run.ID, holderSecond, "")
	m3 := readAttachMode(t, c3)
	if !m3.ReadOnly || m3.Holder == nil || m3.Holder.Principal != holderOwner {
		t.Fatalf("the observer's attach-mode = %+v, want read-only behind the reconnected writer %s", m3, holderOwner)
	}
	waitFor(t, "the observer's session to open", func() bool { return fr2.session(1) != nil })
	observed := fr2.session(1)

	if err := c2.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the reconnected writer's socket: %v", err)
	}

	// The promotion this whole corpus exists to prove keeps working after a
	// rebuild against the same store, not only on a daemon that never restarted.
	m4 := readNextAttachMode(t, c3)
	if m4.ReadOnly || m4.Holder == nil || m4.Holder.Principal != holderSecond {
		t.Fatalf("the observer was not promoted in place after the reconnect: %+v", m4)
	}
	if got := srv2.attachHolderFor(run.ID); got == nil || got.principal != holderSecond {
		t.Fatalf("attachHolderFor = %+v, want the promoted client %s", got, holderSecond)
	}

	wsWrite(t, c3, websocket.MessageBinary, []byte("echo reconnected\r"))
	waitFor(t, "the promoted client's keystrokes to reach the sandbox", func() bool { return observed.written() > 0 })

	waitForActorAudit(t, rec, run.ID, "session.promote", holderSecond)
}
