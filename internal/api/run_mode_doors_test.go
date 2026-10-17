// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The experiences a door must tell apart: "" is a run from an older client and
// stays enterable.
var doorExperiences = []struct {
	name string
	exp  types.RunExperience
}{
	{"interactive", types.ExperienceInteractive},
	{"unset", ""},
}

func TestEstablishExec_BackgroundRunNeverAttaches(t *testing.T) {
	attach := func(exp types.RunExperience) (int32, error) {
		var attaches atomic.Int32
		fr := &sshFakeRunner{attachFn: func() (runner.Session, error) {
			attaches.Add(1)
			return newFakeShellSession(), nil
		}}
		srv := New(Config{BaseCtx: testBaseCtx(t), Runner: fr})
		h := &attachHolder{}
		h.writable.Store(true)
		t.Cleanup(func() { _ = h.mux.Close() })
		run := types.AgentRun{ID: uuid.New(), SandboxRef: "sbx-1", Experience: exp}
		err := srv.establishExec(t.Context(), run, h)
		return attaches.Load(), err
	}

	if n, err := attach(types.ExperienceBackground); !errors.Is(err, errAttachBackgroundOnly) || n != 0 {
		t.Fatalf("background: err=%v attaches=%d, want errAttachBackgroundOnly and none", err, n)
	}
	if got, want := errAttachBackgroundOnly.Error(), runBackgroundOnlyMsg("interactive exec"); got != want {
		t.Errorf("refusal = %q, want %q", got, want)
	}
	for _, tc := range doorExperiences {
		if n, err := attach(tc.exp); err != nil || n != 1 {
			t.Errorf("%s: err=%v attaches=%d, want a clean single attach", tc.name, err, n)
		}
	}
}

func TestSSHAuth_BackgroundRunRefusedForEveryKey(t *testing.T) {
	now := time.Now()
	st := newSSHMemStore()
	alicePriv, alicePub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(alicePub), Principal: "alice@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(alicePub))})
	adminPriv, adminPub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(adminPub), Principal: "root@example.com",
		PublicKey: string(ssh.MarshalAuthorizedKey(adminPub)), Role: oidc.RoleAdmin, RoleCheckedAt: &now})
	run := func(exp types.RunExperience, operatorOwned bool) uuid.UUID {
		r := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", OperatorOwned: operatorOwned,
			State: types.RunRunning, SandboxRef: "sbx-1", Experience: exp}
		st.putRun(r)
		return r.ID
	}
	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	refused := func(t *testing.T, id uuid.UUID, priv ed25519.PrivateKey) {
		t.Helper()
		if _, err := sshDial(t, h, id.String(), priv); err == nil {
			t.Fatal("ssh into a background task succeeded, want refused")
		}
		ev := waitForAudit(t, h.audit, id, "ssh.authenticate", "failure")
		if ev == nil || !strings.Contains(string(ev.Data), reasonRunBackgroundOnly) {
			t.Fatalf("audit = %v, want an ssh.authenticate failure with reason %s; events=%s", ev, reasonRunBackgroundOnly, auditDump(h.audit.snapshot(), id))
		}
	}
	t.Run("the owner's key is refused", func(t *testing.T) {
		refused(t, run(types.ExperienceBackground, false), alicePriv)
	})
	t.Run("a fresh admin key on an operator-owned run is refused", func(t *testing.T) {
		refused(t, run(types.ExperienceBackground, true), adminPriv)
	})
	for _, tc := range doorExperiences {
		t.Run(tc.name+" runs authenticate", func(t *testing.T) {
			for _, c := range []struct {
				priv          ed25519.PrivateKey
				operatorOwned bool
			}{{alicePriv, false}, {adminPriv, true}} {
				client, err := sshDial(t, h, run(tc.exp, c.operatorOwned).String(), c.priv)
				if err != nil {
					t.Fatalf("ssh refused: %v", err)
				}
				_ = client.Close()
			}
		})
	}
}

// readAll drains r in the background so a door that wrongly leaves a channel open
// fails the test instead of hanging it.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { b, err := io.ReadAll(r); ch <- res{b, err} }()
	select {
	case got := <-ch:
		return string(got.b)
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open, want the refusal and a close")
		return ""
	}
}

func TestSSHFreshRun_BackgroundRunRefusesEveryChannel(t *testing.T) {
	want := runBackgroundOnlyMsg("SSH access")
	f := newSSHSyncFixture(t)
	var calls atomic.Int32
	f.fr.mu.Lock()
	f.fr.attachFn = func() (runner.Session, error) { calls.Add(1); return newFakeShellSession(), nil }
	f.fr.execFn = func(runner.ExecSpec) (*runner.ExecSession, error) { calls.Add(1); return fakeEchoExecSession(), nil }
	f.fr.mu.Unlock()
	// The connection was authenticated while the run was enterable; the
	// channels must still refuse once the stored run is a background task.
	bg := f.run
	bg.Experience = types.ExperienceBackground
	f.st.putRun(bg)

	if run, msg := f.h.srv.sshFreshRun(context.Background(), bg.ID, "alice@example.com"); msg != want || run.ID != uuid.Nil {
		t.Fatalf("sshFreshRun = %+v %q, want the background sentence %q", run, msg, want)
	}

	channel := func(t *testing.T, start func(*ssh.Session) error) {
		t.Helper()
		sess, err := f.client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		defer sess.Close()
		stderr, err := sess.StderrPipe()
		if err != nil {
			t.Fatalf("stderr pipe: %v", err)
		}
		if _, err := sess.StdinPipe(); err != nil {
			t.Fatalf("stdin pipe: %v", err)
		}
		if err := start(sess); err != nil {
			t.Fatalf("start: %v", err)
		}
		if got := readAll(t, stderr); !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	}
	t.Run("shell", func(t *testing.T) { channel(t, (*ssh.Session).Shell) })
	t.Run("exec", func(t *testing.T) { channel(t, func(s *ssh.Session) error { return s.Start("true") }) })
	t.Run("sftp", func(t *testing.T) {
		channel(t, func(s *ssh.Session) error { return s.RequestSubsystem("sftp") })
	})
	t.Run("wardyn-sync", func(t *testing.T) {
		channel(t, func(s *ssh.Session) error {
			if err := s.Setenv("WARDYN_SYNC_DIR", "/home/agent/work"); err != nil {
				return err
			}
			if err := s.Setenv("WARDYN_SYNC_DIRECTION", "pull"); err != nil {
				return err
			}
			return s.RequestSubsystem(sshSyncSubsystem)
		})
	})
	t.Run("direct-tcpip", func(t *testing.T) {
		_, err := f.client.Dial("tcp", "127.0.0.1:9999")
		var oce *ssh.OpenChannelError
		if !errors.As(err, &oce) || oce.Message != want {
			t.Fatalf("forward = %v, want an open-channel refusal %q", err, want)
		}
	})
	if n := calls.Load(); n != 0 {
		t.Errorf("the runner was reached %d times by a background task's channels, want 0", n)
	}
}

func TestUIGateway_BackgroundRunRefusedAtEnterAndRelay(t *testing.T) {
	background := func(h *uiHarness) {
		h.run.Experience = types.ExperienceBackground
		h.store.putRun(h.run)
	}
	enter := func(h *uiHarness) *httptest.ResponseRecorder {
		return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}})
	}

	t.Run("enter answers 409 and sets nothing", func(t *testing.T) {
		h := newUIHarness(t, okBackend())
		background(h)
		rec := enter(h)
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusConflict || body.Reason != reasonRunBackgroundOnly || body.Error != runBackgroundOnlyMsg("web application gateway") {
			t.Fatalf("enter = %d %s, want 409 %s", rec.Code, rec.Body.String(), reasonRunBackgroundOnly)
		}
		if got := setUICookies(rec); len(got) != 0 || rec.Header().Get("Location") != "" {
			t.Errorf("a refused enter set a relay cookie %v / location %q", got, rec.Header().Get("Location"))
		}
		if rows := h.audit.auditRows("ui.authorize", "denied"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), reasonRunBackgroundOnly) {
			t.Errorf("ui.authorize denied rows = %+v, want one %s", rows, reasonRunBackgroundOnly)
		}
	})

	t.Run("a minted session is refused by uiDial and by the relay re-assert", func(t *testing.T) {
		h := newUIHarness(t, okBackend())
		background(h)
		now := time.Now()
		session := func(issued int64) uiSession {
			return uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: h.owner,
				Expires: now.Add(time.Hour).Unix(), IssuedAt: issued, AuthorizedAt: now.Unix()}
		}
		ctx := context.WithValue(context.Background(), uiSessionCtxKey{}, session(1))
		_, err := h.srv.uiDial(ctx, "tcp", h.run.ID.String()+":"+strconv.Itoa(uiTestPort))
		var de *uiDialError
		if !errors.As(err, &de) || de.status != http.StatusConflict || de.reason != reasonRunBackgroundOnly {
			t.Fatalf("uiDial = %v, want 409 %s", err, reasonRunBackgroundOnly)
		}
		if de := h.srv.uiReassertRelay(context.Background(), session(2)); de == nil || de.status != http.StatusConflict || de.reason != reasonRunBackgroundOnly {
			t.Fatalf("uiReassertRelay = %+v, want 409 %s", de, reasonRunBackgroundOnly)
		}
	})

	for _, tc := range doorExperiences {
		t.Run(tc.name+" runs still open", func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			h.run.Experience = tc.exp
			h.store.putRun(h.run)
			if rec := enter(h); rec.Code != http.StatusFound {
				t.Fatalf("enter = %d %s, want 302", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestBackgroundRefusalFollowsEntryAuthorization: the refusal never answers
// before ownership, so a caller with no right to the run learns nothing from it.
func TestBackgroundRefusalFollowsEntryAuthorization(t *testing.T) {
	t.Run("UI enter", func(t *testing.T) {
		answer := func(exp types.RunExperience, principal, role string) (int, string) {
			h := newUIHarness(t, okBackend())
			h.run.Experience = exp
			h.store.putRun(h.run)
			rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, principal, role)}})
			return rec.Code, rec.Body.String()
		}
		for _, c := range []struct{ principal, role string }{{"mallory", oidc.RoleUser}, {"root", oidc.RoleAdmin}} {
			wantCode, wantBody := answer(types.ExperienceInteractive, c.principal, c.role)
			gotCode, gotBody := answer(types.ExperienceBackground, c.principal, c.role)
			if gotCode != http.StatusForbidden || gotCode != wantCode || gotBody != wantBody {
				t.Errorf("%s/%s: background = %d %s, interactive = %d %s; want the same 403",
					c.principal, c.role, gotCode, gotBody, wantCode, wantBody)
			}
		}
	})

	t.Run("SSH auth", func(t *testing.T) {
		st := newSSHMemStore()
		malloryPriv, malloryPub := mustSSHKeypair(t)
		st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(malloryPub), Principal: "mallory@example.com",
			PublicKey: string(ssh.MarshalAuthorizedKey(malloryPub))})
		h := newSSHTestHarness(t, st, &sshFakeRunner{})
		failure := func(exp types.RunExperience) string {
			r := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1", Experience: exp}
			st.putRun(r)
			if _, err := sshDial(t, h, r.ID.String(), malloryPriv); err == nil {
				t.Fatal("a foreign key authenticated")
			}
			ev := waitForAudit(t, h.audit, r.ID, "ssh.authenticate", "failure")
			if ev == nil {
				t.Fatalf("no ssh.authenticate failure; events=%s", auditDump(h.audit.snapshot(), r.ID))
			}
			return string(bytes.TrimSpace(ev.Data))
		}
		want, got := failure(types.ExperienceInteractive), failure(types.ExperienceBackground)
		if got != want || strings.Contains(got, reasonRunBackgroundOnly) {
			t.Errorf("background audit = %s, interactive = %s; want identical and silent about the run's shape", got, want)
		}
	})
}
