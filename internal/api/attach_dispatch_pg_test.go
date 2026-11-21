// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatchAttachRunner is holderTestRunner with each Attach's start recorded and an optional stall, and a thaw.
type dispatchAttachRunner struct {
	*holderTestRunner
	mu       sync.Mutex
	attaches []time.Time
	thaws    []time.Time
	stall    func(ctx context.Context) // runs inside Attach; it may outlive ctx
	thaw     func(ctx context.Context) error
	late     []*countingShellSession // sessions Attach returned after a stall
}

func (r *dispatchAttachRunner) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	r.mu.Lock()
	r.attaches = append(r.attaches, time.Now())
	stall := r.stall
	r.mu.Unlock()
	if stall != nil {
		stall(ctx)
	}
	sess, err := r.holderTestRunner.Attach(ctx, ref, opts)
	if stall != nil {
		r.mu.Lock()
		r.late = append(r.late, sess.(*countingShellSession))
		r.mu.Unlock()
	}
	return sess, err
}

func (r *dispatchAttachRunner) FreezeSandbox(context.Context, string) error { return nil }

func (r *dispatchAttachRunner) ThawSandbox(ctx context.Context, _ string) error {
	r.mu.Lock()
	r.thaws = append(r.thaws, time.Now())
	thaw := r.thaw
	r.mu.Unlock()
	if thaw != nil {
		return thaw(ctx)
	}
	return nil
}

func atOrAfter(ts []time.Time, by time.Time) int {
	n := 0
	for _, t := range ts {
		if !t.Before(by) {
			n++
		}
	}
	return n
}

// pausingAttachStore is the attach rig's store as a run pauser that reports the run paused.
type pausingAttachStore struct{ issuerAttachStore }

func (pausingAttachStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Now(), nil
}
func (pausingAttachStore) StampRunActive(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (pausingAttachStore) MarkRunPaused(context.Context, uuid.UUID, types.PauseReason, *time.Time) (bool, error) {
	return false, nil
}
func (pausingAttachStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) { return true, nil }
func (pausingAttachStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

// newDispatchAttachRig is the issuer attach rig with the dispatch bound shortened and the recording runner.
func newDispatchAttachRig(t *testing.T) (*issuerAttachRig, *dispatchAttachRunner) {
	t.Helper()
	rig := newIssuerAttachRig(t)
	dr := &dispatchAttachRunner{holderTestRunner: rig.runner}
	rig.srv.cfg.Runner = dr
	setDispatchFresh(t, dispatchFresh)
	return rig, dr
}

// frames reads c until it closes and counts its binary (sandbox output) frames and its text frames.
func frames(c *websocket.Conn, within time.Duration) (binary, text int) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	for {
		typ, _, err := c.Read(ctx)
		if err != nil {
			return binary, text
		}
		if typ == websocket.MessageBinary {
			binary++
		} else {
			text++
		}
	}
}

func TestAttachIssuerPortal_LateThawRefused_PG(t *testing.T) {
	rig, dr := newDispatchAttachRig(t)
	st := rig.srv.cfg.Store.(issuerAttachStore)
	ast := st.authorityUnits.touchCountingStore.authzStore
	now := time.Now().UTC()
	ast.mu.Lock()
	run := ast.runs[rig.run.ID]
	run.PausedAt, run.PausedReason = &now, types.PauseIdle
	ast.runs[rig.run.ID] = run
	ast.mu.Unlock()
	rig.srv.cfg.Store = pausingAttachStore{st}
	issuerAdmitWaitNS.Store(int64(10 * time.Second)) // the opening reads' own bound is past the dispatch time
	t.Cleanup(func() { issuerAdmitWaitNS.Store(0) })
	var by time.Time
	dr.thaw = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() } // stalls until it is cut
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), rig.via), rig.srv.cfg.Store, rig.run.ID, types.ActorHuman,
		rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	by = time.Now().Add(dispatchFresh)
	url := rig.ts.URL + "/api/v1/runs/" + rig.run.ID.String() + "/attach?ticket=" + tok
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	if resp.StatusCode != http.StatusBadGateway || eb.Reason != reasonAttachResumeFailed {
		t.Fatalf("a thaw stalled past the dispatch time: %d %s, want the resume-failed refusal", resp.StatusCode, body)
	}
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if n := atOrAfter(dr.thaws, by); n != 0 || len(dr.attaches) != 0 {
		t.Fatalf("%d thaws at or after the dispatch time, %d attaches; want 0, 0", n, len(dr.attaches))
	}
}

func TestAttachIssuerPortal_LateOpeningRefusedBeforeServe_PG(t *testing.T) {
	rig, dr := newDispatchAttachRig(t)
	release := make(chan struct{})
	by := time.Now().Add(dispatchFresh)
	dr.stall = func(context.Context) { <-release } // the opening returns its session late, whatever its context
	c := rig.dial(t)
	time.Sleep(time.Until(by) + 200*time.Millisecond)
	close(release)
	binary, _ := frames(c, 10*time.Second)
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if n := atOrAfter(dr.attaches, by); n != 0 {
		t.Fatalf("%d openings started at or after the dispatch time", n)
	}
	if binary != 0 {
		t.Fatalf("%d sandbox output frames from an opening returned late, want 0", binary)
	}
	for _, s := range dr.late {
		waitWithin(t, 5*time.Second, "the late session closed by its identity", s.isClosed)
	}
}

func TestAttachIssuerPortal_OpenedSessionOutlivesDeadline_PG(t *testing.T) {
	rig, _ := newDispatchAttachRig(t)
	by := time.Now().Add(dispatchFresh)
	writer := rig.dial(t)
	observer := rig.dial(t)
	waitWithin(t, 5*time.Second, "both openings", func() bool {
		rig.runner.mu.Lock()
		defer rig.runner.mu.Unlock()
		return len(rig.runner.sessions) >= 2
	})
	// The observer still receives output after the dispatch time (scaled: 2 bounds past it).
	time.Sleep(time.Until(by) + 2*dispatchFresh)
	rig.runner.mu.Lock()
	sessions := append([]*countingShellSession(nil), rig.runner.sessions...)
	rig.runner.mu.Unlock()
	for _, s := range sessions {
		go s.feed("output after the dispatch time\r\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, _, err := observer.Read(ctx)
		if err != nil {
			t.Fatalf("the observer's output after the dispatch time: %v", err)
		}
		if typ == websocket.MessageBinary {
			break
		}
	}
	// An idle writer's first keystroke, after the dispatch time, reaches the exec.
	if err := writer.Write(ctx, websocket.MessageBinary, []byte("k")); err != nil {
		t.Fatalf("the writer's first keystroke: %v", err)
	}
	waitWithin(t, 5*time.Second, "the keystroke at the exec", func() bool {
		for _, s := range sessions {
			if s.writtenBytes() > 0 {
				return true
			}
		}
		return false
	})
	// A revoke after a timely opening is torn down by the watch, as before.
	at := rig.suspend(t)
	closed := make(chan time.Time, 1)
	go func() {
		for {
			if _, _, err := writer.Read(context.Background()); err != nil {
				closed <- time.Now()
				return
			}
		}
	}()
	select {
	case got := <-closed:
		cutBy(t, "terminal opened in time, suspend after its dispatch time", at, got, relayEvery+relayBound+relaySlack)
	case <-time.After(10 * time.Second):
		t.Fatal("the socket outlived a suspend after a timely opening")
	}
}

func TestAttachIssuerPortal_LocalTransportRefused_PG(t *testing.T) {
	rig, dr := newDispatchAttachRig(t)
	rig.srv.cfg.AttachLocal = true
	ask := func(via *types.DelegationVia) (int, errorBody) {
		ctx := context.Background()
		if via != nil {
			ctx = audit.WithDelegation(ctx, *via)
		}
		tok, err := mintAttachTicket(ctx, rig.srv.cfg.Store, rig.run.ID, types.ActorHuman, rig.person, oidc.RoleUser, oidc.Issue{At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, rig.ts.URL+"/api/v1/runs/"+rig.run.ID.String()+"/attach?transport=local&local_nonce=n&ticket="+tok, nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var eb errorBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return resp.StatusCode, eb
	}
	code, eb := ask(&rig.via)
	if code != http.StatusConflict || eb.Reason != reasonAttachLocalUnavailable || eb.Detail != localAttachIssuerPortal {
		t.Fatalf("an issuer portal's local attach: %d %+v, want 409 with detail %q", code, eb, localAttachIssuerPortal)
	}
	dr.mu.Lock()
	n := len(dr.attaches)
	dr.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d openings for a refused local attach, want 0", n)
	}
	// A ticket with no portal on it is not refused for being a portal's.
	if _, eb := ask(nil); eb.Detail == localAttachIssuerPortal {
		t.Fatalf("a plain ticket's local attach was refused as an issuer portal's: %+v", eb)
	}
}
