// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *macWindowStore) DeletePrincipalPref(_ context.Context, principal, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.prefs, principal+"\x00"+key)
	return nil
}

func (r *macWindowRig) pendingKept() bool {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	_, ok := r.st.prefs[r.h.owner+"\x00"+macWindowPendingKey+r.h.run.ID.String()]
	return ok
}

// With no test seam, a runner's window end is its remote substrate: it reads the window cap off the runner's
// caps (unreported until they arrive), and a runner whose link is down fails the open as a plain error.
func TestMacWindowSubForIsTheRemoteSubstrate(t *testing.T) {
	r := newMacWindowRig(t)
	r.h.srv.macWindowSubs = nil
	id := *r.h.run.RunnerID
	// The rig's runner is a fake already carrying caps; drop it so the run's runner is the server's own
	// endpoint for a runner that has never connected.
	r.h.srv.runnerStreams.subsMu.Lock()
	delete(r.h.srv.runnerStreams.subs, id)
	r.h.srv.runnerStreams.subsMu.Unlock()
	sub, ok := r.h.srv.macWindowSubFor(id)
	if _, is := sub.(remoteMacWindowSub); !ok || !is {
		t.Fatalf("macWindowSubFor = %T, %v, want the remote substrate's adapter", sub, ok)
	}
	if s, rep := sub.MacWindowSupported(); s || rep {
		t.Fatalf("before caps: supported %v, reported %v", s, rep)
	}
	for window, want := range map[bool]bool{false: false, true: true} {
		caps, err := runnerwire.Marshal(runnerwire.Caps{Version: "test", MacOSVM: &runnerwire.MacVMCaps{Max: 1, Window: window}})
		if err != nil {
			t.Fatal(err)
		}
		r.h.srv.runnerEndpoint(id).sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
		if s, rep := sub.MacWindowSupported(); s != want || !rep {
			t.Fatalf("caps window=%v: supported %v, reported %v", window, s, rep)
		}
	}
	ref := placement.RunnerRefPrefix(id.String()) + "sandbox"
	_, err := sub.MacWindow(context.Background(), ref, macWindowRequest{GrantID: uuid.New()})
	if err == nil || errors.Is(err, errMacWindowNotAtMac) || errors.Is(err, errMacWindowAlreadyOpen) {
		t.Fatalf("an open on a runner whose link is down = %v, want a plain error", err)
	}
}

// The runner's mac_window_unexpected event reaches the audit, for a run of that runner only, and a malformed
// payload is refused as a shape error without a row.
func TestMacWindowUnexpectedEventPath(t *testing.T) {
	r := newMacWindowRig(t)
	ctx := context.Background()
	id := *r.h.run.RunnerID
	data, err := runnerwire.Marshal(runnerwire.MacWindowUnexpected{Grant: "x1", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	ev := func(runID *uuid.UUID, data []byte) runnerwire.Event {
		return runnerwire.Event{Kind: runnerwire.EventMacWindowUnexpected, RunID: runID, Data: data}
	}
	if err := r.h.srv.eventFromRunner(ctx, id, "", ev(&r.h.run.ID, data)); err != nil {
		t.Fatalf("the event = %v", err)
	}
	b := r.rows("takeover.begin")
	if len(b) != 1 || b[0].Outcome != "denied" || rowData(t, b[0])["reason"] != "mac_window_unexpected" || rowData(t, b[0])["grant_id"] != "x1" {
		t.Fatalf("rows = %+v, want one denied row naming grant x1", b)
	}
	for name, bad := range map[string]runnerwire.Event{
		"unknown field":  ev(&r.h.run.ID, []byte(`{"grant":"x1","extra":1}`)),
		"not json":       ev(&r.h.run.ID, []byte(`nope`)),
		"no run":         ev(nil, data),
		"another runner": ev(&r.h.run.ID, data),
	} {
		runner := id
		if name == "another runner" {
			runner = uuid.New()
		}
		if err := r.h.srv.eventFromRunner(ctx, runner, "", bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(r.rows("takeover.begin")); n != 1 {
		t.Fatalf("%d rows after the refused events, want the one", n)
	}
}

// The launch spends the create's pending grant whether the open succeeds or is refused, and spending it does
// not end a window that is open: the re-check judges the person by the grant the window opened under.
func TestMacWindowAtStartSpendsThePendingGrant(t *testing.T) {
	t.Run("opened", func(t *testing.T) {
		r := newMacWindowRig(t)
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		if r.sub.openCount() != 1 || r.pendingKept() {
			t.Fatalf("opens %d, grant kept %v, want one open and no grant", r.sub.openCount(), r.pendingKept())
		}
		r.tick()
		r.tick()
		if r.h.srv.macWindows.open(r.h.run.ID) == nil || len(r.rows("takeover.close")) != 0 {
			t.Fatal("spending the grant ended the open window")
		}
	})
	t.Run("refused", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.sub.err = errMacWindowNotAtMac
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		if r.pendingKept() {
			t.Fatal("a refused launch kept its grant")
		}
	})
}

// A create asking for a window at start is refused, with the not-your-Mac sentence, for a caller who could never
// open it: an operator's run or a delegated token.
func TestMacWindowCreateRefusesWhoCannotOpen(t *testing.T) {
	r := newMacWindowRig(t)
	human := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/runs", nil)
		return req.WithContext(withHumanIdentity(req.Context(), r.h.owner, r.h.owner+"@example.com", oidc.RoleUser, types.UserTypeStandard, nil, false))
	}
	for name, tc := range map[string]struct {
		req     func() *http.Request
		ceiling governanceCeiling
		refused bool
	}{
		"a person":          {req: human},
		"an operator's run": {req: human, ceiling: governanceCeiling{Operator: true}, refused: true},
		"a delegated token": {req: func() *http.Request {
			q := human()
			return q.WithContext(audit.WithDelegation(q.Context(), types.DelegationVia{}))
		}, refused: true},
	} {
		req := &createRunRequest{Agent: "claude-code", MachineKind: "macos_vm", DesktopWindow: "at_start"}
		rf := r.h.srv.macWindowCreateRefusal(context.Background(), tc.req(), tc.ceiling, req, "")
		if refused := rf != nil; refused != tc.refused {
			t.Errorf("%s: refusal %+v, want refused=%v", name, rf, tc.refused)
			continue
		}
		if rf != nil && (rf.body.Reason != reasonMacWindowNotYourMac || rf.body.Error != macWindowNotYourMac("").body.Error) {
			t.Errorf("%s: refusal %+v, want the not-your-Mac sentence", name, rf)
		}
	}
}
